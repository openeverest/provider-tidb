package provider

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// TestMonitoringContract keeps definition/monitoring.yaml, which core matches
// bindings against, in step with what MonitoringSources publishes: every
// component type serves its metrics natively under the kind published here.
func TestMonitoringContract(t *testing.T) {
	var contract corev1alpha1.ProviderMonitoring
	readYAML(t, "../../definition/monitoring.yaml", &contract)
	var definition struct {
		Components map[string]struct {
			Type string `json:"type"`
		} `json:"components"`
	}
	readYAML(t, "../../definition/provider.yaml", &definition)

	if len(contract.Integrations) != 0 {
		t.Errorf("integrations = %v, want none: the provider renders no operator-native integration", contract.Integrations)
	}
	published := map[string]string{}
	for _, cm := range componentMetrics {
		published[definition.Components[cm.component].Type] = cm.kind
	}
	want := map[string]corev1alpha1.MonitoringComponentType{}
	for typ, kind := range published {
		want[typ] = corev1alpha1.MonitoringComponentType{Kind: kind, Metrics: corev1alpha1.MetricsSourceNative}
	}
	if !reflect.DeepEqual(contract.ComponentTypes, want) {
		t.Errorf("componentTypes = %+v, want %+v", contract.ComponentTypes, want)
	}
	if len(published) != len(definition.Components) {
		t.Errorf("metrics published for %d of the %d components", len(published), len(definition.Components))
	}
}

func readyGroups(ready int32, components ...string) []client.Object {
	var objs []client.Object
	for _, component := range components {
		meta := testCluster().ObjectMeta
		meta.UID = ""
		status := tidbcorev1.GroupStatus{ReadyReplicas: ready}
		switch component {
		case common.ComponentPD:
			g := &tidbcorev1.PDGroup{ObjectMeta: meta}
			g.Status.GroupStatus = status
			objs = append(objs, g)
		case common.ComponentTiKV:
			g := &tidbcorev1.TiKVGroup{ObjectMeta: meta}
			g.Status.GroupStatus = status
			objs = append(objs, g)
		case common.ComponentTiDB:
			g := &tidbcorev1.TiDBGroup{ObjectMeta: meta}
			g.Status.GroupStatus = status
			objs = append(objs, g)
		case common.ComponentTiFlash:
			g := &tidbcorev1.TiFlashGroup{ObjectMeta: meta}
			g.Status.GroupStatus = status
			objs = append(objs, g)
		case common.ComponentTiProxy:
			g := &tidbcorev1.TiProxyGroup{ObjectMeta: meta}
			g.Status.GroupStatus = status
			objs = append(objs, g)
		}
	}
	return objs
}

func mtlsCluster() *tidbcorev1.Cluster {
	cluster := testCluster()
	cluster.Spec.TLSCluster = &tidbcorev1.TLSCluster{Enabled: true}
	return cluster
}

func plainEndpoint(component, kind, port, path string) corev1alpha1.MetricsEndpoint {
	return corev1alpha1.MetricsEndpoint{
		Component: component,
		Kind:      kind,
		PodSelector: map[string]string{
			controller.InstanceLabel:  "orders",
			controller.ComponentLabel: component,
		},
		Port:   intstr.FromString(port),
		Path:   path,
		Scheme: "http",
	}
}

func mtlsEndpoint(component, kind, port, path string) corev1alpha1.MetricsEndpoint {
	ep := plainEndpoint(component, kind, port, path)
	ep.Scheme = "https"
	ep.TLS = &corev1alpha1.MetricsEndpointTLS{
		TLSConfig: commonv1alpha1.TLSConfig{CA: &commonv1alpha1.CABundleSource{
			SecretKeyRef: &commonv1alpha1.SecretKeyRef{Name: "orders-cluster-client-secret", Key: "ca.crt"},
		}},
		ServerName:                 "orders-" + component,
		ClientCertificateSecretRef: &commonv1alpha1.SecretRef{Name: "orders-cluster-client-secret"},
	}
	return ep
}

func TestMonitoringSources(t *testing.T) {
	all := []string{common.ComponentPD, common.ComponentTiKV, common.ComponentTiDB, common.ComponentTiFlash, common.ComponentTiProxy}
	tests := []struct {
		name string
		objs []client.Object
		want []corev1alpha1.MetricsEndpoint
	}{
		{name: "no cluster yet"},
		{
			name: "groups without a ready pod",
			objs: append(readyGroups(0, all...), testCluster()),
		},
		{
			name: "plaintext, ready core components only",
			objs: append(append(readyGroups(1, common.ComponentPD, common.ComponentTiKV, common.ComponentTiDB),
				readyGroups(0, common.ComponentTiFlash)...), testCluster()),
			want: []corev1alpha1.MetricsEndpoint{
				plainEndpoint("pd", "tidb.pd", "client", "/metrics"),
				plainEndpoint("tikv", "tikv", "status", "/metrics"),
				plainEndpoint("tidb", "tidb", "status", "/metrics"),
			},
		},
		{
			name: "mutual TLS, every component",
			objs: append(readyGroups(2, all...), mtlsCluster()),
			want: []corev1alpha1.MetricsEndpoint{
				mtlsEndpoint("pd", "tidb.pd", "client", "/metrics"),
				mtlsEndpoint("tikv", "tikv", "status", "/metrics"),
				mtlsEndpoint("tidb", "tidb", "status", "/metrics"),
				mtlsEndpoint("tiflash", "tidb.tiflash", "metrics", "/metrics"),
				mtlsEndpoint("tiflash", "tidb.tiflash", tidbcorev1.TiFlashPortNameProxyStatus, "/metrics"),
				mtlsEndpoint("tiproxy", "tidb.tiproxy", "api", "/api/metrics"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := tlsContext(t, nil, tt.objs...)
			got, err := New().MonitoringSources(c)
			assertError(t, err, "")
			if !reflect.DeepEqual(got.Metrics, tt.want) {
				t.Errorf("metrics =\n%+v\nwant\n%+v", got.Metrics, tt.want)
			}
		})
	}
}

// An error reading the operator objects is returned, so the runtime keeps the
// sources it published last instead of withdrawing them.
func TestMonitoringSourcesReadError(t *testing.T) {
	c, cl := tlsContext(t, nil, append(readyGroups(1, common.ComponentPD), testCluster())...)
	failing := interceptor.NewClient(cl.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*tidbcorev1.PDGroup); ok {
				return errors.New("boom")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	c = controller.NewContext(context.Background(), failing, c.Instance(), common.ProviderName)
	_, err := New().MonitoringSources(c)
	assertError(t, err, "get pd group: boom")
}
