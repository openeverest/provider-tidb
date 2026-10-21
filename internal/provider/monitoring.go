package provider

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"

	vmv1beta1 "github.com/VictoriaMetrics/operator/api/operator/v1beta1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition/topologies/cluster"
	"github.com/openeverest/provider-tidb/internal/common"
)

const (
	metricsPath = "/metrics"

	podMonitorSuffix  = "-metrics"
	vmPodScrapeSuffix = "-vm-metrics"

	// podMonitorLabelsEnv and vmPodScrapeLabelsEnv hold the chart's
	// podMonitor.labels and vmPodScrape.labels as JSON objects.
	podMonitorLabelsEnv  = "POD_MONITOR_LABELS"
	vmPodScrapeLabelsEnv = "VM_POD_SCRAPE_LABELS"

	// componentPodLabelMeta is controller.ComponentLabel as Kubernetes service
	// discovery exposes it to relabeling.
	componentPodLabelMeta = "__meta_kubernetes_pod_label_core_openeverest_io_component"
)

// metricsEndpoint is a component port serving native Prometheus metrics. PD
// serves them on the port TiKV names for gRPC ("client"), so every endpoint
// keeps only the pods of its own component.
type metricsEndpoint struct {
	port      string
	component string
}

var metricsEndpoints = []metricsEndpoint{
	{tidbcorev1.PDPortNameClient, common.ComponentPD},
	{tidbcorev1.TiKVPortNameStatus, common.ComponentTiKV},
	{tidbcorev1.TiDBPortNameStatus, common.ComponentTiDB},
	{tidbcorev1.TiFlashPortNameMetrics, common.ComponentTiFlash},
	{tidbcorev1.TiFlashPortNameProxyStatus, common.ComponentTiFlash},
}

// metricsTLS is how a scrape reaches the metrics endpoints of a cluster with
// mTLS between components: over https, presenting the cluster client
// certificate and verifying each component by its service name, which every
// component certificate carries.
type metricsTLS struct {
	secret string
}

func clusterMetricsTLS(c *controller.Context) *metricsTLS {
	if !tlsSettings(c).Cluster {
		return nil
	}
	return &metricsTLS{secret: clusterClientTLSSecretName(c.Name())}
}

func (t *metricsTLS) key(key string) *corev1.SecretKeySelector {
	return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: t.secret}, Key: key}
}

func componentServerName(instance, component string) string {
	return instance + "-" + component
}

// scraper is one monitoring system the provider can create a scrape object for.
type scraper struct {
	field     string // parameter path, for error messages
	kind      schema.GroupVersionKind
	operator  string
	suffix    string
	labelsEnv string
	// empty returns an object of the kind with only its name set, for deletes.
	empty func(meta metav1.ObjectMeta) client.Object
}

var (
	prometheusScraper = scraper{
		field:     "monitoring.prometheus",
		kind:      monitoringv1.SchemeGroupVersion.WithKind(monitoringv1.PodMonitorsKind),
		operator:  "the Prometheus Operator",
		suffix:    podMonitorSuffix,
		labelsEnv: podMonitorLabelsEnv,
		empty:     func(m metav1.ObjectMeta) client.Object { return &monitoringv1.PodMonitor{ObjectMeta: m} },
	}
	victoriaMetricsScraper = scraper{
		field:     "monitoring.victoriaMetrics",
		kind:      vmv1beta1.GroupVersion.WithKind("VMPodScrape"),
		operator:  "the VictoriaMetrics Operator",
		suffix:    vmPodScrapeSuffix,
		labelsEnv: vmPodScrapeLabelsEnv,
		empty:     func(m metav1.ObjectMeta) client.Object { return &vmv1beta1.VMPodScrape{ObjectMeta: m} },
	}
)

// scrapeSettings is what the Instance asks of one monitoring system.
type scrapeSettings struct {
	interval string
	labels   map[string]string
}

func podMonitorName(instance string) string {
	return instance + podMonitorSuffix
}

func vmPodScrapeName(instance string) string {
	return instance + vmPodScrapeSuffix
}

// monitoringParams returns the Instance's monitoring settings, or nil when
// they are absent or cannot be decoded (Validate reports the latter).
func monitoringParams(c *controller.Context) *cluster.Monitoring {
	var params cluster.ClusterTopologyParameters
	if !c.TryDecodeTopologyParameters(&params) {
		return nil
	}
	return params.Monitoring
}

func prometheusSettings(m *cluster.Monitoring) *scrapeSettings {
	if m == nil || m.Prometheus == nil || !m.Prometheus.Enabled {
		return nil
	}
	return &scrapeSettings{interval: m.Prometheus.Interval, labels: m.Prometheus.PodMonitorLabels}
}

func victoriaMetricsSettings(m *cluster.Monitoring) *scrapeSettings {
	if m == nil || m.VictoriaMetrics == nil || !m.VictoriaMetrics.Enabled {
		return nil
	}
	return &scrapeSettings{interval: m.VictoriaMetrics.Interval, labels: m.VictoriaMetrics.PodScrapeLabels}
}

// validateMonitoring checks the settings of every enabled monitoring system and
// that its operator's CRD is installed.
func validateMonitoring(c *controller.Context) error {
	if topology := c.Instance().Spec.Topology; topology == nil || topology.Parameters == nil || topology.Parameters.Raw == nil {
		return nil
	}
	var params cluster.ClusterTopologyParameters
	if err := c.DecodeTopologyParameters(&params); err != nil {
		return fmt.Errorf("invalid topology parameters: %w", err)
	}
	if settings := prometheusSettings(params.Monitoring); settings != nil {
		if err := validateScraper(c, prometheusScraper, *settings, "podMonitorLabels"); err != nil {
			return err
		}
	}
	if settings := victoriaMetricsSettings(params.Monitoring); settings != nil {
		if err := validateScraper(c, victoriaMetricsScraper, *settings, "podScrapeLabels"); err != nil {
			return err
		}
	}
	return nil
}

func validateScraper(c *controller.Context, s scraper, settings scrapeSettings, labelsField string) error {
	if settings.interval != "" {
		if _, err := model.ParseDuration(settings.interval); err != nil {
			return fmt.Errorf("%s.interval %q is not a valid duration such as 30s or 1m", s.field, settings.interval)
		}
	}
	if err := validateLabels(s.field+"."+labelsField, settings.labels); err != nil {
		return err
	}
	if _, err := c.Client().RESTMapper().RESTMapping(s.kind.GroupKind(), s.kind.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("%s requires %s: the %s CRD (%s) is not installed",
				s.field, s.operator, s.kind.Kind, s.kind.GroupVersion())
		}
		return fmt.Errorf("look up the %s CRD: %w", s.kind.Kind, err)
	}
	return nil
}

func validateLabels(field string, labels map[string]string) error {
	for key, value := range labels {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return fmt.Errorf("%s key %q: %s", field, key, errs[0])
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			return fmt.Errorf("%s[%q] value %q: %s", field, key, value, errs[0])
		}
	}
	return nil
}

// syncMonitoring creates the scrape object of every enabled monitoring system
// and removes those of the disabled ones.
func syncMonitoring(c *controller.Context) error {
	params := monitoringParams(c)
	if err := syncScraper(c, prometheusScraper, prometheusSettings(params), buildPodMonitor); err != nil {
		return err
	}
	return syncScraper(c, victoriaMetricsScraper, victoriaMetricsSettings(params), buildVMPodScrape)
}

func syncScraper(c *controller.Context, s scraper, settings *scrapeSettings,
	build func(*controller.Context, scrapeSettings, map[string]string) client.Object) error {
	name := c.Name() + s.suffix
	if settings == nil {
		err := c.Delete(s.empty(metav1.ObjectMeta{Name: name, Namespace: c.Namespace()}))
		if err != nil && !meta.IsNoMatchError(err) {
			return fmt.Errorf("delete %s %q: %w", s.kind.Kind, name, err)
		}
		return nil
	}

	defaults, err := labelsFromEnv(s.labelsEnv)
	if err != nil {
		return err
	}
	if err := c.Apply(build(c, *settings, defaults)); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("%s requires the %s CRD (%s): %w", s.field, s.kind.Kind, s.kind.GroupVersion(), err)
		}
		return fmt.Errorf("apply %s %q: %w", s.kind.Kind, name, err)
	}
	return nil
}

// labelsFromEnv reads the labels the chart sets for every scrape object.
func labelsFromEnv(env string) (map[string]string, error) {
	value := os.Getenv(env)
	if value == "" {
		return nil, nil
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(value), &labels); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", env, err)
	}
	return labels, nil
}

// scrapeObjectMeta layers the labels: chart defaults, then the Instance's own,
// then the managed labels plugin-metrics discovers the object by.
func scrapeObjectMeta(c *controller.Context, name string, defaults, own map[string]string) metav1.ObjectMeta {
	objectMeta := c.ObjectMeta(name)
	labels := map[string]string{}
	maps.Copy(labels, defaults)
	maps.Copy(labels, own)
	maps.Copy(labels, objectMeta.Labels)
	objectMeta.Labels = labels
	return objectMeta
}

func instancePodSelector(c *controller.Context) metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{
		controller.ProviderLabel: common.ProviderName,
		controller.InstanceLabel: c.Name(),
	}}
}

func buildPodMonitor(c *controller.Context, settings scrapeSettings, defaults map[string]string) client.Object {
	tls := clusterMetricsTLS(c)
	endpoints := make([]monitoringv1.PodMetricsEndpoint, 0, len(metricsEndpoints))
	for _, e := range metricsEndpoints {
		endpoint := monitoringv1.PodMetricsEndpoint{
			Port:     ptr.To(e.port),
			Path:     metricsPath,
			Interval: monitoringv1.Duration(settings.interval),
			RelabelConfigs: []monitoringv1.RelabelConfig{{
				Action:       "keep",
				SourceLabels: []monitoringv1.LabelName{componentPodLabelMeta},
				Regex:        e.component,
			}},
		}
		if tls != nil {
			endpoint.Scheme = ptr.To(monitoringv1.Scheme("https"))
			endpoint.TLSConfig = &monitoringv1.SafeTLSConfig{
				CA:         monitoringv1.SecretOrConfigMap{Secret: tls.key(tlsKeyCA)},
				Cert:       monitoringv1.SecretOrConfigMap{Secret: tls.key(tlsKeyCert)},
				KeySecret:  tls.key(tlsKeyKey),
				ServerName: ptr.To(componentServerName(c.Name(), e.component)),
			}
		}
		endpoints = append(endpoints, endpoint)
	}
	return &monitoringv1.PodMonitor{
		ObjectMeta: scrapeObjectMeta(c, podMonitorName(c.Name()), defaults, settings.labels),
		Spec: monitoringv1.PodMonitorSpec{
			Selector:            instancePodSelector(c),
			PodTargetLabels:     []string{controller.ComponentLabel},
			PodMetricsEndpoints: endpoints,
		},
	}
}

func buildVMPodScrape(c *controller.Context, settings scrapeSettings, defaults map[string]string) client.Object {
	tls := clusterMetricsTLS(c)
	endpoints := make([]vmv1beta1.PodMetricsEndpoint, 0, len(metricsEndpoints))
	for _, e := range metricsEndpoints {
		endpoint := vmv1beta1.PodMetricsEndpoint{
			Port: ptr.To(e.port),
			EndpointScrapeParams: vmv1beta1.EndpointScrapeParams{
				Path:     metricsPath,
				Interval: settings.interval,
			},
			EndpointRelabelings: vmv1beta1.EndpointRelabelings{
				RelabelConfigs: []*vmv1beta1.RelabelConfig{{
					Action:       "keep",
					SourceLabels: []string{componentPodLabelMeta},
					Regex:        vmv1beta1.StringOrArray{e.component},
				}},
			},
		}
		if tls != nil {
			endpoint.Scheme = "https"
			endpoint.TLSConfig = &vmv1beta1.TLSConfig{
				CA:         vmv1beta1.SecretOrConfigMap{Secret: tls.key(tlsKeyCA)},
				Cert:       vmv1beta1.SecretOrConfigMap{Secret: tls.key(tlsKeyCert)},
				KeySecret:  tls.key(tlsKeyKey),
				ServerName: componentServerName(c.Name(), e.component),
			}
		}
		endpoints = append(endpoints, endpoint)
	}
	return &vmv1beta1.VMPodScrape{
		ObjectMeta: scrapeObjectMeta(c, vmPodScrapeName(c.Name()), defaults, settings.labels),
		Spec: vmv1beta1.VMPodScrapeSpec{
			Selector:            instancePodSelector(c),
			PodTargetLabels:     []string{controller.ComponentLabel},
			PodMetricsEndpoints: endpoints,
		},
	}
}
