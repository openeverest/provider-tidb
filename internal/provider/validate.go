package provider

import (
	"fmt"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-tidb/internal/common"
)

// maxClusterNameLength mirrors the TiDB Operator Cluster CRD constraint
// (metadata.name must not exceed 37 characters).
const maxClusterNameLength = 37

// ValidateTiDB checks that the Instance spec can be reconciled into a TiDB
// cluster.
func ValidateTiDB(c *controller.Context) error {
	if len(c.Name()) > maxClusterNameLength {
		return fmt.Errorf("instance name %q is too long: TiDB cluster name must not exceed %d characters",
			c.Name(), maxClusterNameLength)
	}

	comps := c.Instance().Spec.Components
	for _, name := range []string{common.ComponentPD, common.ComponentTiKV, common.ComponentTiDB} {
		if _, ok := comps[name]; !ok {
			return fmt.Errorf("required component %q is missing", name)
		}
	}

	// PD forms a quorum (etcd/raft), so an even count is never valid.
	if pd := comps[common.ComponentPD]; pd.Replicas != nil && *pd.Replicas%2 == 0 {
		return fmt.Errorf("pd replicas must be an odd number, got %d", *pd.Replicas)
	}

	return nil
}
