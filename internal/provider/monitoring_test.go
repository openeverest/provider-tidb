package provider

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	vmv1beta1 "github.com/VictoriaMetrics/operator/api/operator/v1beta1"
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

func monitoringTopology(prometheus *cluster.Prometheus, vm *cluster.VictoriaMetrics) cluster.ClusterTopologyParameters {
	return cluster.ClusterTopologyParameters{Monitoring: &cluster.Monitoring{Prometheus: prometheus, VictoriaMetrics: vm}}
}

// scrapeTarget is what one endpoint of a scrape object reads.
type scrapeTarget struct{ port, keep string }

var wantScrapeTargets = []scrapeTarget{
	{tidbcorev1.PDPortNameClient, "pd"},
	{tidbcorev1.TiKVPortNameStatus, "tikv|tidb"},
	{tidbcorev1.TiFlashPortNameMetrics, "tiflash"},
	{tidbcorev1.TiFlashPortNameProxyStatus, "tiflash"},
}

func wantScrapeLabels(release string) map[string]string {
	return map[string]string{
		"release":                      release,
		"team":                         "db",
		"app.kubernetes.io/managed-by": "everest",
		"app.kubernetes.io/instance":   "orders",
		controller.ProviderLabel:       common.ProviderName,
	}
}

var (
	testScrapeSettings = scrapeSettings{
		interval: "30s",
		labels:   map[string]string{"release": "instance-prom", "app.kubernetes.io/instance": "spoofed"},
	}
	testDefaultLabels = map[string]string{"release": "chart", "team": "db"}
	wantPodSelector   = map[string]string{controller.ProviderLabel: common.ProviderName, controller.InstanceLabel: "orders"}
)

func TestBuildPodMonitor(t *testing.T) {
	c := controller.NewContext(context.Background(), fake.NewClientBuilder().Build(), monitoredInstance(t, nil), common.ProviderName)
	pm := buildPodMonitor(c, testScrapeSettings, testDefaultLabels).(*monitoringv1.PodMonitor)

	if pm.Name != "orders-metrics" || pm.Namespace != testNamespace {
		t.Errorf("name = %s/%s, want %s/orders-metrics", pm.Namespace, pm.Name, testNamespace)
	}
	if !reflect.DeepEqual(pm.Labels, wantScrapeLabels("instance-prom")) {
		t.Errorf("labels = %v", pm.Labels)
	}
	if !reflect.DeepEqual(pm.Spec.Selector.MatchLabels, wantPodSelector) {
		t.Errorf("selector = %v", pm.Spec.Selector.MatchLabels)
	}
	if !reflect.DeepEqual(pm.Spec.PodTargetLabels, []string{controller.ComponentLabel}) {
		t.Errorf("podTargetLabels = %v", pm.Spec.PodTargetLabels)
	}
	var got []scrapeTarget
	for _, ep := range pm.Spec.PodMetricsEndpoints {
		if ep.Path != "/metrics" || ep.Interval != "30s" {
			t.Errorf("endpoint %s: path %q interval %q", ptr.Deref(ep.Port, ""), ep.Path, ep.Interval)
		}
		if len(ep.RelabelConfigs) != 1 || ep.RelabelConfigs[0].Action != "keep" ||
			!reflect.DeepEqual(ep.RelabelConfigs[0].SourceLabels, []monitoringv1.LabelName{componentPodLabelMeta}) {
			t.Fatalf("endpoint %s: relabelings = %+v", ptr.Deref(ep.Port, ""), ep.RelabelConfigs)
		}
		got = append(got, scrapeTarget{ptr.Deref(ep.Port, ""), ep.RelabelConfigs[0].Regex})
	}
	if !reflect.DeepEqual(got, wantScrapeTargets) {
		t.Errorf("endpoints = %v, want %v", got, wantScrapeTargets)
	}
}

func TestBuildVMPodScrape(t *testing.T) {
	c := controller.NewContext(context.Background(), fake.NewClientBuilder().Build(), monitoredInstance(t, nil), common.ProviderName)
	ps := buildVMPodScrape(c, testScrapeSettings, testDefaultLabels).(*vmv1beta1.VMPodScrape)

	if ps.Name != "orders-vm-metrics" || ps.Namespace != testNamespace {
		t.Errorf("name = %s/%s, want %s/orders-vm-metrics", ps.Namespace, ps.Name, testNamespace)
	}
	if !reflect.DeepEqual(ps.Labels, wantScrapeLabels("instance-prom")) {
		t.Errorf("labels = %v", ps.Labels)
	}
	if !reflect.DeepEqual(ps.Spec.Selector.MatchLabels, wantPodSelector) {
		t.Errorf("selector = %v", ps.Spec.Selector.MatchLabels)
	}
	if !reflect.DeepEqual(ps.Spec.PodTargetLabels, []string{controller.ComponentLabel}) {
		t.Errorf("podTargetLabels = %v", ps.Spec.PodTargetLabels)
	}
	var got []scrapeTarget
	for _, ep := range ps.Spec.PodMetricsEndpoints {
		if ep.Path != "/metrics" || ep.Interval != "30s" {
			t.Errorf("endpoint %s: path %q interval %q", ptr.Deref(ep.Port, ""), ep.Path, ep.Interval)
		}
		if len(ep.RelabelConfigs) != 1 || ep.RelabelConfigs[0].Action != "keep" ||
			!reflect.DeepEqual(ep.RelabelConfigs[0].SourceLabels, []string{componentPodLabelMeta}) ||
			len(ep.RelabelConfigs[0].Regex) != 1 {
			t.Fatalf("endpoint %s: relabelings = %+v", ptr.Deref(ep.Port, ""), ep.RelabelConfigs)
		}
		got = append(got, scrapeTarget{ptr.Deref(ep.Port, ""), ep.RelabelConfigs[0].Regex[0]})
	}
	if !reflect.DeepEqual(got, wantScrapeTargets) {
		t.Errorf("endpoints = %v, want %v", got, wantScrapeTargets)
	}
}

func TestSyncMonitoring(t *testing.T) {
	tests := []struct {
		name        string
		params      any
		promEnv     string
		vmEnv       string
		wantApplied map[string]map[string]string // kind -> labels expected among its labels
		wantDeleted []string
		wantErr     string
	}{
		{name: "no topology parameters", params: nil,
			wantDeleted: []string{"orders-metrics", "orders-vm-metrics"}},
		{name: "both disabled",
			params:      monitoringTopology(&cluster.Prometheus{}, &cluster.VictoriaMetrics{}),
			wantDeleted: []string{"orders-metrics", "orders-vm-metrics"}},
		{name: "prometheus only, chart labels",
			params:      monitoringTopology(&cluster.Prometheus{Enabled: true}, nil),
			promEnv:     `{"release":"kube-prometheus-stack"}`,
			wantApplied: map[string]map[string]string{"PodMonitor": {"release": "kube-prometheus-stack"}},
			wantDeleted: []string{"orders-vm-metrics"}},
		{name: "victoriametrics only, instance labels win",
			params:      monitoringTopology(nil, &cluster.VictoriaMetrics{Enabled: true, PodScrapeLabels: map[string]string{"vm": "mine"}}),
			vmEnv:       `{"vm":"chart","team":"db"}`,
			wantApplied: map[string]map[string]string{"VMPodScrape": {"vm": "mine", "team": "db"}},
			wantDeleted: []string{"orders-metrics"}},
		{name: "both enabled",
			params: monitoringTopology(&cluster.Prometheus{Enabled: true}, &cluster.VictoriaMetrics{Enabled: true}),
			wantApplied: map[string]map[string]string{
				"PodMonitor":  {"app.kubernetes.io/instance": "orders"},
				"VMPodScrape": {"app.kubernetes.io/instance": "orders"},
			}},
		{name: "invalid chart labels",
			params:  monitoringTopology(nil, &cluster.VictoriaMetrics{Enabled: true}),
			vmEnv:   `not json`,
			wantErr: "parsing VM_POD_SCRAPE_LABELS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(podMonitorLabelsEnv, tt.promEnv)
			t.Setenv(vmPodScrapeLabelsEnv, tt.vmEnv)
			applied := map[string]map[string]any{}
			var deleted []string
			cl := fake.NewClientBuilder().WithScheme(monitoringScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
				Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
					data, err := json.Marshal(obj)
					if err != nil {
						return err
					}
					var body map[string]any
					if err := json.Unmarshal(data, &body); err != nil {
						return err
					}
					applied[body["kind"].(string)] = body
					return nil
				},
				Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
					deleted = append(deleted, obj.GetName())
					return nil
				},
			}).Build()
			c := controller.NewContext(context.Background(), cl, monitoredInstance(t, tt.params), common.ProviderName)

			assertError(t, syncMonitoring(c), tt.wantErr)
			if tt.wantErr != "" {
				return
			}
			slices.Sort(deleted)
			if !slices.Equal(deleted, tt.wantDeleted) {
				t.Errorf("deleted %v, want %v", deleted, tt.wantDeleted)
			}
			if len(applied) != len(tt.wantApplied) {
				t.Errorf("applied kinds %v, want %v", slices.Collect(func(yield func(string) bool) {
					for k := range applied {
						if !yield(k) {
							return
						}
					}
				}), tt.wantApplied)
			}
			for kind, wantLabels := range tt.wantApplied {
				body, ok := applied[kind]
				if !ok {
					t.Errorf("no %s applied", kind)
					continue
				}
				metadata := body["metadata"].(map[string]any)
				labels := metadata["labels"].(map[string]any)
				for k, v := range wantLabels {
					if labels[k] != v {
						t.Errorf("%s label %s = %v, want %s", kind, k, labels[k], v)
					}
				}
				owners, _ := metadata["ownerReferences"].([]any)
				if len(owners) != 1 || owners[0].(map[string]any)["kind"] != "Instance" {
					t.Errorf("%s ownerReferences = %v, want the Instance", kind, owners)
				}
			}
		})
	}
}

func TestValidateTiDBMonitoring(t *testing.T) {
	prom := func(p cluster.Prometheus) any { return monitoringTopology(&p, nil) }
	vm := func(v cluster.VictoriaMetrics) any { return monitoringTopology(nil, &v) }
	tests := []struct {
		name        string
		params      any
		withoutProm bool
		withoutVM   bool
		wantErr     string
	}{
		{name: "no topology parameters", params: nil},
		{name: "prometheus enabled", params: prom(cluster.Prometheus{Enabled: true, Interval: "15s",
			PodMonitorLabels: map[string]string{"release": "kube-prometheus-stack"}}), withoutVM: true},
		{name: "victoriametrics enabled", params: vm(cluster.VictoriaMetrics{Enabled: true, Interval: "1m",
			PodScrapeLabels: map[string]string{"team": "db"}}), withoutProm: true},
		{name: "both enabled", params: monitoringTopology(&cluster.Prometheus{Enabled: true}, &cluster.VictoriaMetrics{Enabled: true})},
		{name: "disabled skips the checks", params: monitoringTopology(&cluster.Prometheus{Interval: "soon"},
			&cluster.VictoriaMetrics{Interval: "soon"}), withoutProm: true, withoutVM: true},
		{name: "invalid prometheus interval", params: prom(cluster.Prometheus{Enabled: true, Interval: "soon"}),
			wantErr: `monitoring.prometheus.interval "soon" is not a valid duration`},
		{name: "invalid victoriametrics interval", params: vm(cluster.VictoriaMetrics{Enabled: true, Interval: "soon"}),
			wantErr: `monitoring.victoriaMetrics.interval "soon" is not a valid duration`},
		{name: "invalid label key", params: prom(cluster.Prometheus{Enabled: true,
			PodMonitorLabels: map[string]string{"bad key": "x"}}),
			wantErr: `monitoring.prometheus.podMonitorLabels key "bad key"`},
		{name: "invalid label value", params: vm(cluster.VictoriaMetrics{Enabled: true,
			PodScrapeLabels: map[string]string{"team": strings.Repeat("x", 64)}}),
			wantErr: `monitoring.victoriaMetrics.podScrapeLabels["team"] value`},
		{name: "prometheus operator missing", params: prom(cluster.Prometheus{Enabled: true}), withoutProm: true,
			wantErr: "monitoring.prometheus requires the Prometheus Operator: the PodMonitor CRD (monitoring.coreos.com/v1) is not installed"},
		{name: "victoriametrics operator missing", params: vm(cluster.VictoriaMetrics{Enabled: true}), withoutVM: true,
			wantErr: "monitoring.victoriaMetrics requires the VictoriaMetrics Operator: the VMPodScrape CRD (operator.victoriametrics.com/v1beta1) is not installed"},
		{name: "malformed parameters", params: map[string]any{"monitoring": "yes"},
			wantErr: "invalid topology parameters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The fake client's default mapper knows no CRDs; map the kinds the
			// installed operators would serve.
			installs := []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, tidbcorev1.Install}
			if !tt.withoutProm {
				installs = append(installs, monitoringv1.AddToScheme)
			}
			if !tt.withoutVM {
				installs = append(installs, vmv1beta1.AddToScheme)
			}
			mapperScheme := runtime.NewScheme()
			for _, install := range installs {
				if err := install(mapperScheme); err != nil {
					t.Fatal(err)
				}
			}
			cl := fake.NewClientBuilder().WithScheme(monitoringScheme(t)).
				WithRESTMapper(testrestmapper.TestOnlyStaticRESTMapper(mapperScheme)).
				WithObjects(testProvider()).Build()
			c := controller.NewContext(context.Background(), cl, monitoredInstance(t, tt.params), common.ProviderName)
			assertError(t, ValidateTiDB(c), tt.wantErr)
		})
	}
}

func monitoringScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{
		corev1alpha1.AddToScheme, tidbcorev1.Install, monitoringv1.AddToScheme, vmv1beta1.AddToScheme,
	} {
		if err := install(scheme); err != nil {
			t.Fatalf("building scheme: %v", err)
		}
	}
	return scheme
}
