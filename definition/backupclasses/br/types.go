// Package br contains the schema-bearing Go types for the "br" BackupClass.
// Each struct is converted to an OpenAPI v3 schema by `provider-sdk generate`
// and inlined into the generated BackupClass manifest.
//
// +k8s:openapi-gen=true
package br

// BrBackupParameters describes per-backup parameters (spec.parameters). A full
// snapshot needs no extra input for now.
type BrBackupParameters struct{}

// BrRestoreParameters describes per-restore parameters (spec.parameters).
// Restore is implemented separately; kept for forward compatibility.
type BrRestoreParameters struct{}
