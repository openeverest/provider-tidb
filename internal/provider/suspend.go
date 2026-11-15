package provider

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"
)

// applySuspend suspends the cluster with TiDB Operator's suspendCompute: the
// operator deletes every pod and keeps the PVCs.
//
// The action is rendered only while a suspend is requested. On resume the
// provider stops applying it, so server-side apply removes it and the
// operator starts the pods again.
func applySuspend(c *controller.Context, cluster *tidbcorev1.Cluster) {
	if !c.Instance().SuspendRequested() {
		return
	}
	cluster.Spec.SuspendAction = &tidbcorev1.SuspendAction{SuspendCompute: true}
}

// suspendStatus reports a cluster asked to suspend: Suspended once TiDB
// Operator reports the cluster suspended and the TiProxy pods are gone, which
// the Cluster condition does not cover; Suspending with the operator's
// progress until then.
func suspendStatus(c *controller.Context) (controller.Status, error) {
	cluster := &tidbcorev1.Cluster{}
	found, err := c.Exists(cluster, c.Name())
	if err != nil {
		return controller.Status{}, fmt.Errorf("get cluster: %w", err)
	}
	if !found || cluster.Spec.SuspendAction == nil || !cluster.Spec.SuspendAction.SuspendCompute {
		return controller.Suspending("waiting for TiDB Operator to start suspending the cluster"), nil
	}
	cond := meta.FindStatusCondition(cluster.Status.Conditions, tidbcorev1.ClusterCondSuspended)
	switch {
	case cond == nil || cond.ObservedGeneration < cluster.Generation:
		return controller.Suspending("waiting for TiDB Operator to start suspending the cluster"), nil
	case cond.Status != metav1.ConditionTrue:
		return controller.Suspending("TiDB Operator: " + cond.Message), nil
	}

	tiproxy := &tidbcorev1.TiProxyGroup{}
	found, err = c.Exists(tiproxy, c.Name())
	if err != nil {
		return controller.Status{}, fmt.Errorf("get tiproxy group: %w", err)
	}
	if found && !meta.IsStatusConditionTrue(tiproxy.Status.Conditions, tidbcorev1.CondSuspended) {
		return controller.Suspending("waiting for the TiProxy pods to stop"), nil
	}
	return controller.Suspended(), nil
}
