package provider

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

var _ controller.MonitoringSourcesProvider = (*Provider)(nil)

const (
	metricsPath = "/metrics"
	// tiproxyMetricsPath is where TiProxy documents its metrics on the API port.
	tiproxyMetricsPath = "/api/metrics"
)

// metricsPort is a component port serving native Prometheus metrics.
type metricsPort struct {
	name string
	path string
}

// componentMetrics lists, per component, the ports serving its metrics, in the
// order they are published. PD serves them on its client port; TiFlash on two
// ports, its own and its embedded TiKV proxy's.
var componentMetrics = []struct {
	component string
	// kind matches the component type's kind in definition/monitoring.yaml.
	kind  string
	ports []metricsPort
	// group returns an empty component group and a reader of its ready pods.
	group func() (client.Object, func() int32)
}{
	{common.ComponentPD, "tidb.pd", []metricsPort{{tidbcorev1.PDPortNameClient, metricsPath}},
		func() (client.Object, func() int32) {
			g := &tidbcorev1.PDGroup{}
			return g, func() int32 { return g.Status.ReadyReplicas }
		}},
	{common.ComponentTiKV, "tikv", []metricsPort{{tidbcorev1.TiKVPortNameStatus, metricsPath}},
		func() (client.Object, func() int32) {
			g := &tidbcorev1.TiKVGroup{}
			return g, func() int32 { return g.Status.ReadyReplicas }
		}},
	{common.ComponentTiDB, "tidb", []metricsPort{{tidbcorev1.TiDBPortNameStatus, metricsPath}},
		func() (client.Object, func() int32) {
			g := &tidbcorev1.TiDBGroup{}
			return g, func() int32 { return g.Status.ReadyReplicas }
		}},
	{common.ComponentTiFlash, "tidb.tiflash", []metricsPort{
		{tidbcorev1.TiFlashPortNameMetrics, metricsPath},
		{tidbcorev1.TiFlashPortNameProxyStatus, metricsPath},
	}, func() (client.Object, func() int32) {
		g := &tidbcorev1.TiFlashGroup{}
		return g, func() int32 { return g.Status.ReadyReplicas }
	}},
	{common.ComponentTiProxy, "tidb.tiproxy", []metricsPort{{tidbcorev1.TiProxyPortNameAPI, tiproxyMetricsPath}},
		func() (client.Object, func() int32) {
			g := &tidbcorev1.TiProxyGroup{}
			return g, func() int32 { return g.Status.ReadyReplicas }
		}},
}

// MonitoringSources implements controller.MonitoringSourcesProvider. Every
// component serves its metrics natively, so the endpoints of a component are
// published, whatever the bound classes require, while its group has a ready
// pod: a rolling restart or scale keeps them. With mutual TLS between the
// components every metrics port requires a client certificate, so the scraper
// presents the cluster client certificate and verifies each component by its
// service name, which every component certificate carries. An error reading
// the operator objects keeps the published sources.
func (p *Provider) MonitoringSources(c *controller.Context) (*corev1alpha1.MonitoringSources, error) {
	sources := &corev1alpha1.MonitoringSources{}
	tidbCluster := &tidbcorev1.Cluster{}
	found, err := c.Exists(tidbCluster, c.Name())
	if err != nil {
		return nil, fmt.Errorf("get cluster: %w", err)
	}
	if !found {
		return sources, nil
	}
	mtls := tidbCluster.Spec.TLSCluster != nil && tidbCluster.Spec.TLSCluster.Enabled

	for _, cm := range componentMetrics {
		group, ready := cm.group()
		if err := c.Get(group, c.Name()); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get %s group: %w", cm.component, err)
		}
		if ready() == 0 {
			continue
		}
		for _, port := range cm.ports {
			endpoint := corev1alpha1.MetricsEndpoint{
				Component: cm.component,
				Kind:      cm.kind,
				PodSelector: map[string]string{
					controller.InstanceLabel:  c.Name(),
					controller.ComponentLabel: cm.component,
				},
				Port:   intstr.FromString(port.name),
				Path:   port.path,
				Scheme: "http",
			}
			if mtls {
				endpoint.Scheme = "https"
				endpoint.TLS = clusterMetricsTLS(c.Name(), cm.component)
			}
			sources.Metrics = append(sources.Metrics, endpoint)
		}
	}
	return sources, nil
}

func clusterMetricsTLS(instance, component string) *corev1alpha1.MetricsEndpointTLS {
	clientSecret := clusterClientTLSSecretName(instance)
	return &corev1alpha1.MetricsEndpointTLS{
		TLSConfig: commonv1alpha1.TLSConfig{
			CA: &commonv1alpha1.CABundleSource{
				SecretKeyRef: &commonv1alpha1.SecretKeyRef{Name: clientSecret, Key: tlsKeyCA},
			},
		},
		ServerName:                 instance + "-" + component,
		ClientCertificateSecretRef: &commonv1alpha1.SecretRef{Name: clientSecret},
	}
}
