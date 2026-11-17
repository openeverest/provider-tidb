package provider

import (
	"fmt"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition/components"
	"github.com/openeverest/provider-tidb/internal/common"
)

const defaultTargetCPUUtilization int32 = 80

// autoscalingFor returns the autoscaling settings of the tidb component, nil
// when it runs a fixed number of replicas.
func autoscalingFor(c *controller.Context, comp corev1alpha1.ComponentSpec) *components.Autoscaling {
	var params components.TidbParameters
	if !c.TryDecodeComponentParameters(comp, &params) {
		return nil
	}
	return params.Autoscaling
}

func autoscalerName(c *controller.Context, component string) string {
	return c.Name() + "-" + component
}

// applyAutoscalable applies a group that a HorizontalPodAutoscaler may scale. With autoscaling the autoscaler owns the replica count between the
// component's replicas and maxReplicas: the group is re-applied with the count
// the autoscaler last chose, since the CRD requires the field. The apply is
// tied to the resourceVersion that count was read at, so a scale decision made
// in between fails the apply instead of being undone.
func applyAutoscalable[G client.Object](
	c *controller.Context, owner *tidbcorev1.Cluster, component string, comp corev1alpha1.ComponentSpec,
	group, existing G, replicas func(G) **int32,
) error {
	autoscaling := autoscalingFor(c, comp)
	minReplicas := **replicas(group)
	if autoscaling == nil {
		if err := deleteAutoscaler(c, component); err != nil {
			return err
		}
		return applyOwnedByCluster(c, owner, group)
	}

	gvk, err := apiutil.GVKForObject(group, c.Client().Scheme())
	if err != nil {
		return fmt.Errorf("resolve %s group kind: %w", component, err)
	}
	if err := c.Apply(buildAutoscaler(c, component, gvk.GroupVersion().String(), gvk.Kind, minReplicas, autoscaling)); err != nil {
		return fmt.Errorf("apply %s autoscaler: %w", component, err)
	}

	found, err := c.Exists(existing, c.Name())
	if err != nil {
		return fmt.Errorf("get %s group: %w", component, err)
	}
	if found {
		current := ptr.Deref(*replicas(existing), minReplicas)
		*replicas(group) = ptr.To(min(max(current, minReplicas), autoscaling.MaxReplicas))
		group.SetResourceVersion(existing.GetResourceVersion())
	}
	if err := applyOwnedByCluster(c, owner, group); err != nil {
		if apierrors.IsConflict(err) {
			return controller.WaitForDuration(fmt.Sprintf("%s group changed while it was synced", component), time.Second)
		}
		return err
	}
	return nil
}

func buildAutoscaler(
	c *controller.Context, component, apiVersion, kind string, minReplicas int32, autoscaling *components.Autoscaling,
) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: c.ObjectMeta(autoscalerName(c, component)),
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: apiVersion, Kind: kind, Name: c.Name()},
			MinReplicas:    &minReplicas,
			MaxReplicas:    autoscaling.MaxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: ptr.To(ptr.Deref(autoscaling.TargetCPUUtilization, defaultTargetCPUUtilization)),
					},
				},
			}},
		},
	}
}

func deleteAutoscaler(c *controller.Context, component string) error {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	found, err := c.Exists(hpa, autoscalerName(c, component))
	if err != nil {
		return fmt.Errorf("get %s autoscaler: %w", component, err)
	}
	if !found || !hpa.DeletionTimestamp.IsZero() {
		return nil
	}
	return c.Delete(hpa)
}

// validateAutoscaling checks TiDB's autoscaling settings. TiProxy cannot be
// autoscaled yet: TiDB Operator v2.0.1 does not publish the pod selector of a
// TiProxyGroup, which a HorizontalPodAutoscaler needs (v2.2.0 does).
func validateAutoscaling(c *controller.Context) error {
	comp := c.Instance().Spec.Components[common.ComponentTiDB]
	autoscaling := autoscalingFor(c, comp)
	if autoscaling == nil {
		return nil
	}
	minReplicas := *replicasOrDefault(comp.Replicas, defaultTiDBReplicas)
	if minReplicas < 1 {
		return fmt.Errorf("tidb autoscaling needs at least 1 replica")
	}
	if autoscaling.MaxReplicas < minReplicas {
		return fmt.Errorf("tidb autoscaling.maxReplicas (%d) must be at least its replicas (%d)",
			autoscaling.MaxReplicas, minReplicas)
	}
	if t := autoscaling.TargetCPUUtilization; t != nil && (*t < 1 || *t > 100) {
		return fmt.Errorf("tidb autoscaling.targetCPUUtilization must be between 1 and 100, got %d", *t)
	}
	// The target is a share of the pods' CPU request, which the operator sets
	// from the limit.
	if toResources(comp.Resources).CPU == nil {
		return fmt.Errorf("tidb autoscaling needs a CPU limit (resources.limits.cpu) to measure utilization against")
	}
	return nil
}
