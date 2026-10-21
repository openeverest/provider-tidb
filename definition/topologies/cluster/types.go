// Package cluster contains the topology parameter type for the cluster topology.
//
// +k8s:openapi-gen=true
package cluster

// ClusterTopologyParameters defines topology-level parameters for the standard
// distributed TiDB cluster. Per-component settings are expressed through
// component specs.
type ClusterTopologyParameters struct {
	// Monitoring configures metrics collection.
	Monitoring *Monitoring `json:"monitoring,omitempty"`
}

// Monitoring configures metrics collection for the instance.
type Monitoring struct {
	// Prometheus configures scraping by the Prometheus Operator.
	Prometheus *Prometheus `json:"prometheus,omitempty"`
	// VictoriaMetrics configures scraping by the VictoriaMetrics Operator.
	VictoriaMetrics *VictoriaMetrics `json:"victoriaMetrics,omitempty"`
}

// Prometheus configures the PodMonitor that scrapes PD, TiKV, TiDB and TiFlash.
type Prometheus struct {
	// Enabled creates the PodMonitor; disabling it removes the PodMonitor.
	Enabled bool `json:"enabled,omitempty"`
	// Interval between scrapes, e.g. 30s. Defaults to the Prometheus global
	// scrape interval.
	Interval string `json:"interval,omitempty"`
	// PodMonitorLabels are added to the PodMonitor so that the Prometheus
	// podMonitorSelector picks it up (e.g. release: kube-prometheus-stack).
	PodMonitorLabels map[string]string `json:"podMonitorLabels,omitempty"`
}

// VictoriaMetrics configures the VMPodScrape that scrapes PD, TiKV, TiDB and
// TiFlash.
type VictoriaMetrics struct {
	// Enabled creates the VMPodScrape; disabling it removes the VMPodScrape.
	Enabled bool `json:"enabled,omitempty"`
	// Interval between scrapes, e.g. 30s. Defaults to the VMAgent's global
	// scrape interval.
	Interval string `json:"interval,omitempty"`
	// PodScrapeLabels are added to the VMPodScrape so that a VMAgent's
	// podScrapeSelector picks it up.
	PodScrapeLabels map[string]string `json:"podScrapeLabels,omitempty"`
}
