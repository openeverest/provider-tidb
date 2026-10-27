// Package provider implements the OpenEverest provider for TiDB.
package provider

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vmv1beta1 "github.com/VictoriaMetrics/operator/api/operator/v1beta1"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"

	tidbbrv1 "github.com/pingcap/tidb-operator/api/v2/br/v1alpha1"
	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// Compile-time checks that Provider implements the required interfaces.
var (
	_ controller.ProviderInterface = (*Provider)(nil)
	_ controller.WatchProvider     = (*Provider)(nil)
	_ controller.BackupProvider    = (*Provider)(nil)
	_ controller.BackupWatcher     = (*Provider)(nil)
	_ controller.RestoreWatcher    = (*Provider)(nil)
)

// Provider implements controller.ProviderInterface for TiDB, translating an
// OpenEverest Instance into TiDB Operator v2 resources (Cluster + component
// groups).
type Provider struct {
	controller.BaseProvider
}

// New creates a new TiDB Provider instance.
func New() *Provider {
	return &Provider{
		BaseProvider: controller.BaseProvider{
			ProviderName: common.ProviderName,
			SchemeFuncs: []func(*runtime.Scheme) error{
				tidbcorev1.Install,
				tidbbrv1.Install,
				monitoringv1.AddToScheme,
				vmv1beta1.AddToScheme,
				cmapi.AddToScheme,
				// The ServiceAccount, Role and RoleBinding the BR jobs run as.
				rbacv1.AddToScheme,
			},
			WatchConfigs: []controller.WatchConfig{
				controller.WatchOwned(&tidbcorev1.Cluster{}),
				// The component groups are owned by the Cluster (not the Instance),
				// so map them back to the Instance by name (group name == Instance name).
				controller.WatchExternal(&tidbcorev1.PDGroup{}, handler.EnqueueRequestsFromMapFunc(instanceForGroup)),
				controller.WatchExternal(&tidbcorev1.TiKVGroup{}, handler.EnqueueRequestsFromMapFunc(instanceForGroup)),
				controller.WatchExternal(&tidbcorev1.TiDBGroup{}, handler.EnqueueRequestsFromMapFunc(instanceForGroup)),
				controller.WatchExternal(&tidbcorev1.TiFlashGroup{}, handler.EnqueueRequestsFromMapFunc(instanceForGroup)),
				controller.WatchExternal(&tidbcorev1.TiProxyGroup{}, handler.EnqueueRequestsFromMapFunc(instanceForGroup)),
				// Instances are owned by their group, not the Instance, so map them back by cluster label.
				controller.WatchExternal(&tidbcorev1.PD{}, handler.EnqueueRequestsFromMapFunc(instanceForCluster)),
				controller.WatchExternal(&tidbcorev1.TiKV{}, handler.EnqueueRequestsFromMapFunc(instanceForCluster)),
				controller.WatchExternal(&tidbcorev1.TiDB{}, handler.EnqueueRequestsFromMapFunc(instanceForCluster)),
				controller.WatchExternal(&tidbcorev1.TiFlash{}, handler.EnqueueRequestsFromMapFunc(instanceForCluster)),
				controller.WatchExternal(&tidbcorev1.TiProxy{}, handler.EnqueueRequestsFromMapFunc(instanceForCluster)),
				// Re-reconcile an Instance when its seeding Restore (.spec.dataSource) changes.
				controller.WatchExternal(&backupv1alpha1.Restore{}, handler.EnqueueRequestsFromMapFunc(instanceForRestore)),
			},
		},
	}
}

// instanceForRestore maps a Restore CR to the Instance it targets, so the
// Instance re-reconciles when its .spec.dataSource seeding restore progresses.
func instanceForRestore(_ context.Context, obj client.Object) []reconcile.Request {
	restore, ok := obj.(*backupv1alpha1.Restore)
	if !ok || restore.Spec.InstanceRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: restore.Spec.InstanceRef.Name}}}
}

// instanceForCluster maps an operator object to the Instance of the same name,
// since the provider names every TiDB Cluster after its Instance.
func instanceForCluster(_ context.Context, obj client.Object) []reconcile.Request {
	cluster := obj.GetLabels()[tidbcorev1.LabelKeyCluster]
	if cluster == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: cluster}}}
}

// instanceForGroup maps a component group to its Instance. The provider names
// every group after the Instance, so the group's own name is the Instance name.
func instanceForGroup(_ context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}}}
}

// Validate checks that the Instance spec is valid for TiDB.
func (p *Provider) Validate(c *controller.Context) error {
	return ValidateTiDB(c)
}

// Sync reconciles the Instance into the TiDB Operator v2 resources.
func (p *Provider) Sync(c *controller.Context) error {
	return SyncTiDB(c)
}

// Status computes the current status of the TiDB cluster.
func (p *Provider) Status(c *controller.Context) (controller.Status, error) {
	return StatusTiDB(c)
}

// Cleanup deletes the TiDB Cluster and holds the Instance until TiDB Operator
// has torn it down. The operator finds a cluster's groups and members by name,
// so an Instance created again under the same name while the old cluster was
// still being torn down had its new TiKV taken offline by that teardown.
func (p *Provider) Cleanup(c *controller.Context) error {
	cluster := &tidbcorev1.Cluster{}
	found, err := c.Exists(cluster, c.Name())
	if err != nil {
		return fmt.Errorf("get cluster: %w", err)
	}
	left := 0
	if found {
		left++
		if cluster.DeletionTimestamp.IsZero() {
			if err := c.Delete(cluster); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete cluster: %w", err)
			}
		}
	}
	for _, group := range []client.Object{&tidbcorev1.PDGroup{}, &tidbcorev1.TiKVGroup{}, &tidbcorev1.TiDBGroup{},
		&tidbcorev1.TiFlashGroup{}, &tidbcorev1.TiProxyGroup{}} {
		found, err := c.Exists(group, c.Name())
		if err != nil {
			return fmt.Errorf("get %T: %w", group, err)
		}
		if found {
			left++
		}
	}
	inCluster := client.MatchingLabels{tidbcorev1.LabelKeyCluster: c.Name()}
	for _, list := range []client.ObjectList{&tidbcorev1.PDList{}, &tidbcorev1.TiKVList{}, &tidbcorev1.TiDBList{},
		&tidbcorev1.TiFlashList{}, &tidbcorev1.TiProxyList{}} {
		if err := c.List(list, inCluster); err != nil {
			return fmt.Errorf("list %T: %w", list, err)
		}
		left += meta.LenList(list)
	}
	if left > 0 {
		return controller.WaitFor(fmt.Sprintf("waiting for TiDB Operator to tear down cluster %q (%d objects left)", c.Name(), left))
	}
	return nil
}
