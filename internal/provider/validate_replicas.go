package provider

import (
	"fmt"

	"github.com/BurntSushi/toml"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// defaultMaxReplicas is PD's default replication.max-replicas: the number of
// copies it keeps of every region.
const defaultMaxReplicas = 3

// validateTiKVScaleIn rejects removing TiKV nodes when fewer would remain than
// the copies PD keeps of every region. PD refuses to delete such a store, so
// the operator would retry the scale-in forever with the node half-removed.
// Clusters already smaller than that (e.g. single-node dev clusters) can still
// be created and scaled out.
func validateTiKVScaleIn(c *controller.Context) error {
	tikv := &tidbcorev1.TiKVGroup{}
	found, err := c.Exists(tikv, c.Name())
	if err != nil {
		return fmt.Errorf("reading current tikv replicas: %w", err)
	}
	if !found || tikv.Spec.Replicas == nil {
		return nil
	}

	comps := c.Instance().Spec.Components
	current := *tikv.Spec.Replicas
	requested := *replicasOrDefault(comps[common.ComponentTiKV].Replicas, defaultTiKVReplicas)
	if requested >= current {
		return nil
	}

	maxReplicas, err := pdMaxReplicas(componentConfig(c, comps[common.ComponentPD]))
	if err != nil {
		return err
	}
	if int64(requested) < maxReplicas {
		return fmt.Errorf("scaling tikv in from %d to %d replicas is not supported: PD keeps %d copies of every region "+
			"(replication.max-replicas), so it cannot remove a TiKV node once fewer than %d would remain",
			current, requested, maxReplicas, maxReplicas)
	}
	return nil
}

// pdMaxReplicas reads replication.max-replicas from the PD config TOML. PD only
// takes it from the file when the cluster bootstraps; a later change through
// pd-ctl or SQL is not visible here.
func pdMaxReplicas(config string) (int64, error) {
	var cfg struct {
		Replication struct {
			MaxReplicas int64 `toml:"max-replicas"`
		} `toml:"replication"`
	}
	if _, err := toml.Decode(config, &cfg); err != nil {
		return 0, fmt.Errorf("pd config is not valid TOML: %w", err)
	}
	if cfg.Replication.MaxReplicas <= 0 {
		return defaultMaxReplicas, nil
	}
	return cfg.Replication.MaxReplicas, nil
}
