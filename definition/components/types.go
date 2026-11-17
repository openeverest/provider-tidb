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
	// Autoscaling lets a HorizontalPodAutoscaler scale TiDB between the
	// component's replicas and MaxReplicas.
	Autoscaling *Autoscaling `json:"autoscaling,omitempty"`
}

// TiflashParameters defines the parameters for tiflash (columnar storage) components.
type TiflashParameters struct {
	// Config is the inline TOML configuration file for TiFlash.
	Config string `json:"config,omitempty"`
}

// TiproxyParameters defines the parameters for tiproxy (connection proxy) components.
type TiproxyParameters struct {
	// Config is the inline TOML configuration file for TiProxy.
	Config string `json:"config,omitempty"`
}

// Autoscaling configures a HorizontalPodAutoscaler for TiDB. The component's
// replicas are the minimum. The target is a percentage of the
// component's CPU limit, so the component needs one, and the Kubernetes
// metrics API (metrics-server) must be available.
type Autoscaling struct {
	// MaxReplicas is the most pods the autoscaler may run.
	MaxReplicas int32 `json:"maxReplicas"`
	// TargetCPUUtilization is the average CPU utilization to keep, in percent.
	// Defaults to 80.
	TargetCPUUtilization *int32 `json:"targetCPUUtilization,omitempty"`
}
