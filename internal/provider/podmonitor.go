package provider

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition/topologies/cluster"
	"github.com/openeverest/provider-tidb/internal/common"
)

const (
	podMonitorSuffix = "-metrics"
	metricsPath      = "/metrics"

	// podMonitorLabelsEnv holds the chart's podMonitor.labels as a JSON object.
	podMonitorLabelsEnv = "POD_MONITOR_LABELS"

	// componentPodLabelMeta is controller.ComponentLabel as Kubernetes service
	// discovery exposes it to relabeling.
	componentPodLabelMeta = "__meta_kubernetes_pod_label_core_openeverest_io_component"
)

func podMonitorName(instance string) string {
	return instance + podMonitorSuffix
}

// prometheusParams returns the Instance's Prometheus settings, or nil when
// they are absent or cannot be decoded (Validate reports the latter).
func prometheusParams(c *controller.Context) *cluster.Prometheus {
	var params cluster.ClusterTopologyParameters
	if !c.TryDecodeTopologyParameters(&params) || params.Monitoring == nil {
		return nil
	}
	return params.Monitoring.Prometheus
}

// validateMonitoring checks the Prometheus settings and that the Prometheus
// Operator's PodMonitor CRD is installed when scraping is enabled.
func validateMonitoring(c *controller.Context) error {
	if topology := c.Instance().Spec.Topology; topology == nil || topology.Parameters == nil || topology.Parameters.Raw == nil {
		return nil
	}
	var params cluster.ClusterTopologyParameters
	if err := c.DecodeTopologyParameters(&params); err != nil {
		return fmt.Errorf("invalid topology parameters: %w", err)
	}
	if params.Monitoring == nil || params.Monitoring.Prometheus == nil || !params.Monitoring.Prometheus.Enabled {
		return nil
	}

	prometheus := params.Monitoring.Prometheus
	if prometheus.Interval != "" {
		if _, err := model.ParseDuration(prometheus.Interval); err != nil {
			return fmt.Errorf("monitoring.prometheus.interval %q is not a valid duration such as 30s or 1m", prometheus.Interval)
		}
	}
	if err := validateLabels("monitoring.prometheus.podMonitorLabels", prometheus.PodMonitorLabels); err != nil {
		return err
	}

	gvk := monitoringv1.SchemeGroupVersion.WithKind(monitoringv1.PodMonitorsKind)
	if _, err := c.Client().RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("prometheus monitoring requires the Prometheus Operator: the PodMonitor CRD (monitoring.coreos.com/v1) is not installed")
		}
		return fmt.Errorf("look up the PodMonitor CRD: %w", err)
	}
	return nil
}

func validateLabels(field string, labels map[string]string) error {
	for key, value := range labels {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return fmt.Errorf("%s key %q: %s", field, key, errs[0])
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			return fmt.Errorf("%s[%q] value %q: %s", field, key, value, errs[0])
		}
	}
	return nil
}

// syncPodMonitor creates the PodMonitor that scrapes the instance's components
// while Prometheus monitoring is enabled, and removes it otherwise.
func syncPodMonitor(c *controller.Context) error {
	name := podMonitorName(c.Name())
	params := prometheusParams(c)
	if params == nil || !params.Enabled {
		err := c.Delete(&monitoringv1.PodMonitor{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace()}})
		if err != nil && !meta.IsNoMatchError(err) {
			return fmt.Errorf("delete PodMonitor %q: %w", name, err)
		}
		return nil
	}

	defaults, err := defaultPodMonitorLabels()
	if err != nil {
		return err
	}
	if err := c.Apply(buildPodMonitor(c, params, defaults)); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("prometheus monitoring requires the Prometheus Operator PodMonitor CRD (monitoring.coreos.com/v1): %w", err)
		}
		return fmt.Errorf("apply PodMonitor %q: %w", name, err)
	}
	return nil
}

// defaultPodMonitorLabels reads the chart's podMonitor.labels.
func defaultPodMonitorLabels() (map[string]string, error) {
	value := os.Getenv(podMonitorLabelsEnv)
	if value == "" {
		return nil, nil
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(value), &labels); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", podMonitorLabelsEnv, err)
	}
	return labels, nil
}

// buildPodMonitor scrapes every component's native metrics endpoint. Each
// endpoint keeps only its own component's pods: PD serves metrics on the port
// TiKV names for gRPC ("client"). Labels layer as chart defaults, then the
// Instance's own, then the managed labels plugin-metrics discovers it by.
func buildPodMonitor(c *controller.Context, params *cluster.Prometheus, defaults map[string]string) *monitoringv1.PodMonitor {
	objectMeta := c.ObjectMeta(podMonitorName(c.Name()))
	labels := map[string]string{}
	maps.Copy(labels, defaults)
	maps.Copy(labels, params.PodMonitorLabels)
	maps.Copy(labels, objectMeta.Labels)
	objectMeta.Labels = labels

	interval := monitoringv1.Duration(params.Interval)
	endpoint := func(port string, components ...string) monitoringv1.PodMetricsEndpoint {
		return monitoringv1.PodMetricsEndpoint{
			Port:     ptr.To(port),
			Path:     metricsPath,
			Interval: interval,
			RelabelConfigs: []monitoringv1.RelabelConfig{{
				Action:       "keep",
				SourceLabels: []monitoringv1.LabelName{componentPodLabelMeta},
				Regex:        strings.Join(components, "|"),
			}},
		}
	}

	return &monitoringv1.PodMonitor{
		ObjectMeta: objectMeta,
		Spec: monitoringv1.PodMonitorSpec{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{
				controller.ProviderLabel: common.ProviderName,
				controller.InstanceLabel: c.Name(),
			}},
			PodTargetLabels: []string{controller.ComponentLabel},
			PodMetricsEndpoints: []monitoringv1.PodMetricsEndpoint{
				endpoint(tidbcorev1.PDPortNameClient, common.ComponentPD),
				endpoint(tidbcorev1.TiKVPortNameStatus, common.ComponentTiKV, common.ComponentTiDB),
				endpoint(tidbcorev1.TiFlashPortNameMetrics, common.ComponentTiFlash),
				endpoint(tidbcorev1.TiFlashPortNameProxyStatus, common.ComponentTiFlash),
			},
		},
	}
}
