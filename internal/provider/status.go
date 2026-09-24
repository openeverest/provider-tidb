package provider

import (
	"fmt"
	"strconv"
	"strings"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// StatusTiDB reports the Instance phase from the state of the component groups.
// The cluster is Ready once PD, TiKV and TiDB have all converged on their spec.
// Until then it is Provisioning on first rollout, and Updating when a change
// is rolling out on a cluster that was already serving.
func StatusTiDB(c *controller.Context) (controller.Status, error) {
	pd := &tidbcorev1.PDGroup{}
	if err := c.Get(pd, c.Name()); err != nil {
		return controller.Provisioning("Waiting for PD group to be created"), nil
	}
	tikv := &tidbcorev1.TiKVGroup{}
	if err := c.Get(tikv, c.Name()); err != nil {
		return controller.Provisioning("Waiting for TiKV group to be created"), nil
	}
	tidb := &tidbcorev1.TiDBGroup{}
	if err := c.Get(tidb, c.Name()); err != nil {
		return controller.Provisioning("Waiting for TiDB group to be created"), nil
	}

	pending := unconvergedComponents([]groupRollout{
		{common.ComponentPD, pd.Spec.Replicas, pd.Spec.Template.Spec.Version, pd.Status.CommonStatus, pd.Status.GroupStatus},
		{common.ComponentTiKV, tikv.Spec.Replicas, tikv.Spec.Template.Spec.Version, tikv.Status.CommonStatus, tikv.Status.GroupStatus},
		{common.ComponentTiDB, tidb.Spec.Replicas, tidb.Spec.Template.Spec.Version, tidb.Status.CommonStatus, tidb.Status.GroupStatus},
	})
	if len(pending) == 0 {
		return controller.ReadyWithConnectionDetails(connectionDetails(c)), nil
	}
	return rolloutStatus(c.Instance().Status.Phase, pending), nil
}

// groupRollout pairs a component group's desired state with what the operator
// last reported for it.
type groupRollout struct {
	component    string
	replicas     *int32
	version      string
	commonStatus tidbcorev1.CommonStatus
	groupStatus  tidbcorev1.GroupStatus
}

// converged mirrors the operator's IsGroupHealthyAndUpToDate minus observedGeneration,
// which churns while Context.Apply is a full Update (openeverest#3240).
func (g groupRollout) converged() bool {
	if g.replicas == nil || *g.replicas == 0 {
		return false
	}
	desired := *g.replicas
	s := g.groupStatus
	return s.Replicas == desired &&
		s.ReadyReplicas == desired &&
		s.UpdatedReplicas == desired &&
		s.CurrentReplicas == desired &&
		g.commonStatus.UpdateRevision == g.commonStatus.CurrentRevision &&
		s.Version == g.version
}

func unconvergedComponents(groups []groupRollout) []string {
	var pending []string
	for _, g := range groups {
		if !g.converged() {
			pending = append(pending, g.component)
		}
	}
	return pending
}

// rolloutStatus tells a first rollout (Provisioning) apart from a change on a
// cluster that was already serving (Updating).
func rolloutStatus(previous corev1alpha1.InstancePhase, pending []string) controller.Status {
	components := strings.Join(pending, ", ")
	if previous == corev1alpha1.InstancePhaseReady || previous == corev1alpha1.InstancePhaseUpdating {
		return controller.Updating(fmt.Sprintf("Rolling out changes to %s", components))
	}
	return controller.Provisioning(fmt.Sprintf("Waiting for %s to become ready", components))
}

// connectionDetails points at the TiDB internal (SQL) service, which the
// operator names "<group>-tidb" and exposes on the MySQL client port.
func connectionDetails(c *controller.Context) controller.ConnectionDetails {
	return controller.ConnectionDetails{
		Type:     "mysql",
		Provider: common.ProviderName,
		Host:     fmt.Sprintf("%s-tidb.%s", c.Name(), c.Namespace()),
		Port:     strconv.Itoa(tidbcorev1.DefaultTiDBPortClient),
		Username: rootUser,
		Password: readRootPassword(c),
	}
}
