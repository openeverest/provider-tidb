package provider

import (
	"fmt"
	"strconv"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// StatusTiDB reports the Instance phase from the state of the component groups.
// The cluster is Ready only once PD, TiKV and TiDB all have their desired
// replicas ready.
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

	if !groupReady(pd.Spec.Replicas, pd.Status.ReadyReplicas) {
		return controller.Provisioning("Waiting for PD nodes to become ready"), nil
	}
	if !groupReady(tikv.Spec.Replicas, tikv.Status.ReadyReplicas) {
		return controller.Provisioning("Waiting for TiKV nodes to become ready"), nil
	}
	if !groupReady(tidb.Spec.Replicas, tidb.Status.ReadyReplicas) {
		return controller.Provisioning("Waiting for TiDB nodes to become ready"), nil
	}

	return controller.ReadyWithConnectionDetails(connectionDetails(c)), nil
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

func groupReady(desired *int32, ready int32) bool {
	if desired == nil || *desired == 0 {
		return false
	}
	return ready >= *desired
}
