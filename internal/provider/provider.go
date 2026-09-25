// Package provider implements the OpenEverest provider for TiDB.
package provider

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// Compile-time checks that Provider implements the required interfaces.
var (
	_ controller.ProviderInterface = (*Provider)(nil)
	_ controller.WatchProvider     = (*Provider)(nil)
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
			},
			WatchConfigs: []controller.WatchConfig{
				controller.WatchOwned(&tidbcorev1.Cluster{}),
				controller.WatchOwned(&tidbcorev1.PDGroup{}),
				controller.WatchOwned(&tidbcorev1.TiKVGroup{}),
				controller.WatchOwned(&tidbcorev1.TiDBGroup{}),
				// Instances are owned by their group, not the Instance, so map them back by cluster label.
				controller.WatchExternal(&tidbcorev1.PD{}, handler.EnqueueRequestsFromMapFunc(instanceForCluster)),
				controller.WatchExternal(&tidbcorev1.TiKV{}, handler.EnqueueRequestsFromMapFunc(instanceForCluster)),
				controller.WatchExternal(&tidbcorev1.TiDB{}, handler.EnqueueRequestsFromMapFunc(instanceForCluster)),
			},
		},
	}
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

// Cleanup deletes provider-managed resources. The Cluster and component groups
// carry owner references to the Instance, so they are garbage collected
// automatically; no explicit cleanup is required.
func (p *Provider) Cleanup(_ *controller.Context) error {
	return nil
}
