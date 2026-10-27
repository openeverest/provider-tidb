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
// The runtime counts the pods labelled via Context.PodLabels into status.components.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// TiDB Operator v2 resources managed by this provider:
// +kubebuilder:rbac:groups=core.pingcap.com,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.pingcap.com,resources=clusters/status,verbs=get
// +kubebuilder:rbac:groups=core.pingcap.com,resources=pdgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.pingcap.com,resources=pdgroups/status,verbs=get
// +kubebuilder:rbac:groups=core.pingcap.com,resources=tikvgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.pingcap.com,resources=tikvgroups/status,verbs=get
// +kubebuilder:rbac:groups=core.pingcap.com,resources=tidbgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.pingcap.com,resources=tidbgroups/status,verbs=get
// +kubebuilder:rbac:groups=core.pingcap.com,resources=tiflashgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.pingcap.com,resources=tiflashgroups/status,verbs=get
// +kubebuilder:rbac:groups=core.pingcap.com,resources=tiproxygroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.pingcap.com,resources=tiproxygroups/status,verbs=get
// Per-instance CRs the operator creates; their conditions explain unhealthy pods.
// +kubebuilder:rbac:groups=core.pingcap.com,resources=pds;tikvs;tidbs;tiflashes;tiproxies,verbs=get;list;watch

// Backup: OpenEverest backup resources the runtime reconciles.
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backupclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backupstorages,verbs=get;list;watch
// The backup scheduler creates Backups for schedule slots and deletes those past retention.
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups/finalizers,verbs=update
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores/finalizers,verbs=update
// TiDB Operator BR backups the provider creates from a Backup CR.
// +kubebuilder:rbac:groups=br.pingcap.com,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=br.pingcap.com,resources=backups/status,verbs=get;list;watch;update
// TiDB Operator BR restores the provider creates from a Restore CR.
// +kubebuilder:rbac:groups=br.pingcap.com,resources=restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=br.pingcap.com,resources=restores/status,verbs=get;list;watch;update
// The account BR jobs run as (see backup_rbac.go). The provider holds every
// permission it grants there, so it needs no escalate or bind.
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch

// Prometheus Operator PodMonitor the provider creates while monitoring is enabled.
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=podmonitors,verbs=get;list;watch;create;update;patch;delete
// VictoriaMetrics Operator VMPodScrape the provider creates while monitoring is enabled.
// +kubebuilder:rbac:groups=operator.victoriametrics.com,resources=vmpodscrapes,verbs=get;list;watch;create;update;patch;delete
// cert-manager Issuers and Certificates the provider creates when tls.certManager is used.
// +kubebuilder:rbac:groups=cert-manager.io,resources=issuers;certificates,verbs=get;list;watch;create;update;patch;delete
