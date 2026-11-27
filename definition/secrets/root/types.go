// Package root contains the schema-bearing Go types for the "root" secret
// type. Each struct is converted to an OpenAPI v3 schema by
// `provider-sdk generate` and inlined into the generated provider spec.
//
// +k8s:openapi-gen=true
package root

// RootSecretData is the data of a Secret referenced by spec.userSecretRef.
type RootSecretData struct {
	// Password is the password the root user gets when the cluster first
	// bootstraps. It must not be empty or contain control characters.
	Password string `json:"password"`
}
