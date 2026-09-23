// Package cluster contains the topology parameter type for the cluster topology.
//
// +k8s:openapi-gen=true
package cluster

// ClusterTopologyParameters defines topology-level parameters for the standard
// distributed TiDB cluster. There are none for the MVP; per-component settings
// are expressed through component specs.
type ClusterTopologyParameters struct{}
