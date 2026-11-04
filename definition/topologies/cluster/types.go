// Package cluster contains the topology parameter type for the cluster topology.
//
// +k8s:openapi-gen=true
package cluster

// ClusterTopologyParameters defines topology-level parameters for the standard
// distributed TiDB cluster. Per-component settings are expressed through
// component specs.
type ClusterTopologyParameters struct {
	// TLS configures encryption. Both settings are chosen when the Instance is
	// created and cannot be changed afterwards.
	TLS *TLS `json:"tls,omitempty"`
}

// TLS configures encryption. By default the provider issues the certificates
// from a CA it generates for the Instance; CertManager hands that to
// cert-manager instead.
type TLS struct {
	// Cluster encrypts and mutually authenticates the traffic between PD, TiKV,
	// TiDB, TiFlash and TiProxy.
	Cluster bool `json:"cluster,omitempty"`
	// Client lets MySQL clients connect to TiDB and TiProxy over TLS; TiProxy
	// then reaches TiDB over TLS too.
	Client bool `json:"client,omitempty"`
	// CertManager issues the certificates with cert-manager. Chosen when the
	// Instance is created, like the settings above.
	CertManager *CertManager `json:"certManager,omitempty"`
}

// CertManager issues the Instance's certificates with cert-manager, which must
// be installed in the cluster.
type CertManager struct {
	// Enabled issues the certificates with cert-manager. Without IssuerRef the
	// provider sets up a self-signed CA in cert-manager for the Instance.
	Enabled bool `json:"enabled,omitempty"`
	// IssuerRef names the Issuer (in the Instance's namespace) or ClusterIssuer
	// that signs the certificates; setting it implies Enabled. It must put the
	// CA into the certificate Secrets (ca.crt), as CA and Vault issuers do.
	IssuerRef *IssuerRef `json:"issuerRef,omitempty"`
}

// IssuerRef names a cert-manager issuer.
type IssuerRef struct {
	// Name of the Issuer or ClusterIssuer.
	Name string `json:"name,omitempty"`
	// Kind is Issuer (default) or ClusterIssuer.
	Kind string `json:"kind,omitempty"`
}
