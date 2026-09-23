// Package common defines shared constants used across the provider.
package common

const (
	// ProviderName is this provider's identity: the name of the Provider CR it
	// ships and the value Instances put in spec.providerRef.name. It must match
	// `name` in definition/provider.yaml — the runtime uses it to fetch the
	// Provider CR, so a mismatch means nothing ever reconciles.
	ProviderName = "tidb"

	// Component names — the logical roles, matching definition/provider.yaml
	// and the paths in definition/topologies/*/topology.yaml.
	ComponentPD   = "pd"
	ComponentTiKV = "tikv"
	ComponentTiDB = "tidb"

	// TopologyCluster is the standard distributed TiDB topology.
	TopologyCluster = "cluster"
)
