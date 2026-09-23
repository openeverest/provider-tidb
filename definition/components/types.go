// Package components contains parameter types for provider component types.
//
// Each struct here corresponds to a component type defined in versions.yaml
// and is converted to an OpenAPI schema during generation.
// Add fields when a component type accepts parameters beyond
// what the base Instance spec provides.
//
// +k8s:openapi-gen=true
package components

// PdParameters defines the parameters for pd (Placement Driver) components.
type PdParameters struct {
	// Config is the inline TOML configuration file for PD.
	Config string `json:"config,omitempty"`
}

// TikvParameters defines the parameters for tikv (storage engine) components.
type TikvParameters struct {
	// Config is the inline TOML configuration file for TiKV.
	Config string `json:"config,omitempty"`
}

// TidbParameters defines the parameters for tidb (SQL layer) components.
type TidbParameters struct {
	// Config is the inline TOML configuration file for TiDB.
	Config string `json:"config,omitempty"`
}
