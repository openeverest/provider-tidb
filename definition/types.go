// Package definition contains shared types used across the provider definition.
//
// +k8s:openapi-gen=true
package definition

// Parameters defines instance-wide parameters (Instance.spec.parameters).
type Parameters struct {
	// TLS configures encryption in transit: between PD, TiKV, TiDB, TiFlash and
	// TiProxy, and between MySQL clients and TiDB or TiProxy.
	// +optional
	TLS *TLSParameters `json:"tls,omitempty"`
}

// TLSMode controls whether TLS is on and whether MySQL clients must use it.
// +enum
type TLSMode string

const (
	// TLSModeDisabled keeps every connection plaintext. The default. TiDB
	// Operator cannot switch a running cluster between plaintext and TLS, so
	// it can only be chosen when the Instance is created.
	TLSModeDisabled TLSMode = "disabled"
	// TLSModePrefer encrypts and mutually authenticates the traffic between
	// the components, and lets MySQL clients connect over TLS or plaintext.
	TLSModePrefer TLSMode = "preferTLS"
	// TLSModeRequire is preferTLS, and TiDB refuses plaintext MySQL clients,
	// also through TiProxy (require_secure_transport).
	TLSModeRequire TLSMode = "requireTLS"
)

// TLSParameters configures TLS for the instance. The certificates come from,
// in order of precedence: the cert-manager issuer, or else a CA created for
// the instance, through cert-manager when it is installed and by the provider
// otherwise. TiDB Operator mounts certificates from Secrets with fixed names,
// so the instance can't use a certificate Secret of your own. The certificate
// source is chosen when the Instance is created.
type TLSParameters struct {
	// Mode is disabled, preferTLS or requireTLS. Only preferTLS and
	// requireTLS can be switched on a running instance.
	// +default="disabled"
	// +optional
	Mode TLSMode `json:"mode,omitempty"`

	// Issuer is the cert-manager issuer that signs every certificate, instead
	// of a CA created for the instance. It must put its CA into the
	// certificate Secrets (ca.crt), as CA and Vault issuers do. Chosen when
	// the Instance is created.
	// +optional
	Issuer *IssuerReference `json:"issuer,omitempty"`

	// AdditionalNames are DNS names the MySQL certificates of TiDB and TiProxy
	// are valid for besides those of their Services, such as an external name
	// clients connect to. Changing them on a running instance reissues the
	// certificates and restarts TiDB, after maintenance approval.
	// +optional
	AdditionalNames []string `json:"additionalNames,omitempty"`
}

// IssuerKind is the kind of a cert-manager issuer.
// +enum
type IssuerKind string

const (
	// IssuerKindIssuer is an Issuer in the instance namespace.
	IssuerKindIssuer IssuerKind = "Issuer"
	// IssuerKindClusterIssuer is a cluster-wide ClusterIssuer.
	IssuerKindClusterIssuer IssuerKind = "ClusterIssuer"
)

// IssuerReference refers to a cert-manager issuer.
type IssuerReference struct {
	// Name of the issuer.
	Name string `json:"name"`

	// Kind of the issuer: Issuer or ClusterIssuer, or the kind of an external
	// issuer of group.
	// +default="Issuer"
	// +optional
	Kind IssuerKind `json:"kind,omitempty"`

	// Group is the API group of an external issuer, such as
	// awspca.cert-manager.io.
	// +default="cert-manager.io"
	// +optional
	Group string `json:"group,omitempty"`
}
