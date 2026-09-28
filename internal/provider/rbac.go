package provider

// Run `make manifests` to regenerate config/rbac/role.yaml from these markers.
// This file contains kubebuilder RBAC markers for controller-gen.
// See: https://book.kubebuilder.io/reference/markers/rbac

// Base RBAC (required by all providers):
// +kubebuilder:rbac:groups=core.openeverest.io,resources=instances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core.openeverest.io,resources=instances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.openeverest.io,resources=instances/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.openeverest.io,resources=providers,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// The runtime writes the connection details returned by Status() into a Secret.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// The provider creates a bootstrap-sql ConfigMap that sets the root password.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// TiDB Operator v2 resources managed by this provider:
// +kubebuilder:rbac:groups=core.pingcap.com,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.pingcap.com,resources=clusters/status,verbs=get
// +kubebuilder:rbac:groups=core.pingcap.com,resources=pdgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.pingcap.com,resources=pdgroups/status,verbs=get
// +kubebuilder:rbac:groups=core.pingcap.com,resources=tikvgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.pingcap.com,resources=tikvgroups/status,verbs=get
// +kubebuilder:rbac:groups=core.pingcap.com,resources=tidbgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.pingcap.com,resources=tidbgroups/status,verbs=get
// Per-instance CRs the operator creates; their conditions explain unhealthy pods.
// +kubebuilder:rbac:groups=core.pingcap.com,resources=pds;tikvs;tidbs,verbs=get;list;watch

// Backup: OpenEverest backup resources the runtime reconciles.
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backupclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backupstorages,verbs=get;list;watch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups/finalizers,verbs=update
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores/finalizers,verbs=update
// TiDB Operator BR backups the provider creates from a Backup CR.
// +kubebuilder:rbac:groups=br.pingcap.com,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=br.pingcap.com,resources=backups/status,verbs=get
// TiDB Operator BR restores the provider creates from a Restore CR.
// +kubebuilder:rbac:groups=br.pingcap.com,resources=restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=br.pingcap.com,resources=restores/status,verbs=get

