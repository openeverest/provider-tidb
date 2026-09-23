// Package provider implements the OpenEverest provider for TiDB.
package provider

import (
	"k8s.io/apimachinery/pkg/runtime"

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
			},
		},
	}
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
