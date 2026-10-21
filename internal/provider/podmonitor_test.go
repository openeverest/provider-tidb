package provider

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition/topologies/cluster"
	"github.com/openeverest/provider-tidb/internal/common"
)

func monitoredInstance(t *testing.T, params any) *corev1alpha1.Instance {
	t.Helper()
	in := testInstance("8.5.2")
	in.Spec.Topology = &corev1alpha1.TopologySpec{Type: "cluster"}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		in.Spec.Topology.Parameters = &runtime.RawExtension{Raw: raw}
	}
	return in
}

func prometheusTopology(p cluster.Prometheus) cluster.ClusterTopologyParameters {
	return cluster.ClusterTopologyParameters{Monitoring: &cluster.Monitoring{Prometheus: &p}}
}

func TestBuildPodMonitor(t *testing.T) {
	in := monitoredInstance(t, nil)
	c := controller.NewContext(context.Background(), fake.NewClientBuilder().Build(), in, common.ProviderName)
	params := &cluster.Prometheus{
		Enabled:          true,
		Interval:         "30s",
		PodMonitorLabels: map[string]string{"release": "instance-prom", "app.kubernetes.io/instance": "spoofed"},
	}
	defaults := map[string]string{"release": "kube-prometheus-stack", "team": "db"}

	pm := buildPodMonitor(c, params, defaults)

	if pm.Name != "orders-metrics" || pm.Namespace != testNamespace {
		t.Errorf("name = %s/%s, want %s/orders-metrics", pm.Namespace, pm.Name, testNamespace)
	}
	wantLabels := map[string]string{
		"release":                      "instance-prom",
		"team":                         "db",
		"app.kubernetes.io/managed-by": "everest",
		"app.kubernetes.io/instance":   "orders",
		controller.ProviderLabel:       common.ProviderName,
	}
	if !reflect.DeepEqual(pm.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", pm.Labels, wantLabels)
	}
	wantSelector := map[string]string{controller.ProviderLabel: common.ProviderName, controller.InstanceLabel: "orders"}
	if !reflect.DeepEqual(pm.Spec.Selector.MatchLabels, wantSelector) {
		t.Errorf("selector = %v, want %v", pm.Spec.Selector.MatchLabels, wantSelector)
	}
	if !reflect.DeepEqual(pm.Spec.PodTargetLabels, []string{controller.ComponentLabel}) {
		t.Errorf("podTargetLabels = %v", pm.Spec.PodTargetLabels)
	}

	type scrape struct{ port, keep string }
	var got []scrape
	for _, ep := range pm.Spec.PodMetricsEndpoints {
		if ep.Path != "/metrics" || ep.Interval != "30s" {
			t.Errorf("endpoint %s: path %q interval %q, want /metrics every 30s", ptr.Deref(ep.Port, ""), ep.Path, ep.Interval)
		}
		if len(ep.RelabelConfigs) != 1 || ep.RelabelConfigs[0].Action != "keep" ||
			!reflect.DeepEqual(ep.RelabelConfigs[0].SourceLabels, []monitoringv1.LabelName{componentPodLabelMeta}) {
			t.Fatalf("endpoint %s: relabelings = %+v, want one keep on the component label", ptr.Deref(ep.Port, ""), ep.RelabelConfigs)
		}
		got = append(got, scrape{ptr.Deref(ep.Port, ""), ep.RelabelConfigs[0].Regex})
	}
	want := []scrape{
		{tidbcorev1.PDPortNameClient, "pd"},
		{tidbcorev1.TiKVPortNameStatus, "tikv|tidb"},
		{tidbcorev1.TiFlashPortNameMetrics, "tiflash"},
		{tidbcorev1.TiFlashPortNameProxyStatus, "tiflash"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("endpoints = %v, want %v", got, want)
	}
}

func TestSyncPodMonitor(t *testing.T) {
	tests := []struct {
		name       string
		params     any
		envLabels  string
		wantApply  bool
		wantLabels map[string]string
		wantErr    string
	}{
		{name: "no topology parameters", params: nil},
		{name: "monitoring disabled", params: prometheusTopology(cluster.Prometheus{Enabled: false})},
		{name: "enabled with chart labels", params: prometheusTopology(cluster.Prometheus{Enabled: true}),
			envLabels: `{"release":"kube-prometheus-stack"}`, wantApply: true,
			wantLabels: map[string]string{"release": "kube-prometheus-stack"}},
		{name: "instance labels win over chart labels",
			params:    prometheusTopology(cluster.Prometheus{Enabled: true, PodMonitorLabels: map[string]string{"release": "mine"}}),
			envLabels: `{"release":"kube-prometheus-stack"}`, wantApply: true,
			wantLabels: map[string]string{"release": "mine"}},
		{name: "invalid chart labels", params: prometheusTopology(cluster.Prometheus{Enabled: true}),
			envLabels: `not json`, wantErr: "parsing POD_MONITOR_LABELS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(podMonitorLabelsEnv, tt.envLabels)
			var applied map[string]any
			var deleted []string
			cl := fake.NewClientBuilder().WithScheme(monitoringScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
				Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
					data, err := json.Marshal(obj)
					if err != nil {
						return err
					}
					return json.Unmarshal(data, &applied)
				},
				Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
					deleted = append(deleted, obj.GetName())
					return nil
				},
			}).Build()
			c := controller.NewContext(context.Background(), cl, monitoredInstance(t, tt.params), common.ProviderName)

			assertError(t, syncPodMonitor(c), tt.wantErr)
			if tt.wantErr != "" {
				return
			}
			if !tt.wantApply {
				if applied != nil {
					t.Errorf("applied %v, want no PodMonitor", applied["metadata"])
				}
				if !reflect.DeepEqual(deleted, []string{"orders-metrics"}) {
					t.Errorf("deleted %v, want the PodMonitor removed", deleted)
				}
				return
			}
			if applied == nil || applied["kind"] != "PodMonitor" {
				t.Fatalf("applied %v, want a PodMonitor", applied)
			}
			metadata := applied["metadata"].(map[string]any)
			labels := metadata["labels"].(map[string]any)
			for k, v := range tt.wantLabels {
				if labels[k] != v {
					t.Errorf("label %s = %v, want %s", k, labels[k], v)
				}
			}
			owners, _ := metadata["ownerReferences"].([]any)
			if len(owners) != 1 || owners[0].(map[string]any)["kind"] != "Instance" {
				t.Errorf("ownerReferences = %v, want the Instance", owners)
			}
		})
	}
}

func TestValidateTiDBMonitoring(t *testing.T) {
	tests := []struct {
		name        string
		params      any
		withoutCRDs bool
		wantErr     string
	}{
		{name: "no topology parameters", params: nil},
		{name: "enabled", params: prometheusTopology(cluster.Prometheus{Enabled: true, Interval: "15s",
			PodMonitorLabels: map[string]string{"release": "kube-prometheus-stack"}})},
		{name: "disabled skips the checks", params: prometheusTopology(cluster.Prometheus{Interval: "soon"}), withoutCRDs: true},
		{name: "invalid interval", params: prometheusTopology(cluster.Prometheus{Enabled: true, Interval: "soon"}),
			wantErr: `monitoring.prometheus.interval "soon" is not a valid duration`},
		{name: "invalid label key", params: prometheusTopology(cluster.Prometheus{Enabled: true,
			PodMonitorLabels: map[string]string{"bad key": "x"}}),
			wantErr: `monitoring.prometheus.podMonitorLabels key "bad key"`},
		{name: "invalid label value", params: prometheusTopology(cluster.Prometheus{Enabled: true,
			PodMonitorLabels: map[string]string{"release": strings.Repeat("x", 64)}}),
			wantErr: `monitoring.prometheus.podMonitorLabels["release"] value`},
		{name: "prometheus operator missing", params: prometheusTopology(cluster.Prometheus{Enabled: true}), withoutCRDs: true,
			wantErr: "the PodMonitor CRD (monitoring.coreos.com/v1) is not installed"},
		{name: "malformed parameters", params: map[string]any{"monitoring": "yes"},
			wantErr: "invalid topology parameters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := monitoringScheme(t)
			// The fake client's default mapper knows no CRDs; map the kinds an
			// installed Prometheus Operator would serve, or leave them out.
			mapperScheme := scheme
			if tt.withoutCRDs {
				mapperScheme = runtime.NewScheme()
				for _, install := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, tidbcorev1.Install} {
					if err := install(mapperScheme); err != nil {
						t.Fatal(err)
					}
				}
			}
			mapper := testrestmapper.TestOnlyStaticRESTMapper(mapperScheme)
			cl := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(testProvider()).Build()
			c := controller.NewContext(context.Background(), cl, monitoredInstance(t, tt.params), common.ProviderName)
			assertError(t, ValidateTiDB(c), tt.wantErr)
		})
	}
}

func monitoringScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, tidbcorev1.Install, monitoringv1.AddToScheme} {
		if err := install(scheme); err != nil {
			t.Fatalf("building scheme: %v", err)
		}
	}
	return scheme
}
