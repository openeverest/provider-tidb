package provider

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"
	metav1alpha1 "github.com/pingcap/tidb-operator/api/v2/meta/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

const testTiProxyVersion = "v1.3.3"

func TestEnabledTiProxy(t *testing.T) {
	replicas := func(n int32) *int32 { return &n }
	tests := []struct {
		name  string
		comps map[string]corev1alpha1.ComponentSpec
		want  bool
	}{
		{"absent", map[string]corev1alpha1.ComponentSpec{}, false},
		{"zero replicas", map[string]corev1alpha1.ComponentSpec{common.ComponentTiProxy: {Replicas: replicas(0)}}, false},
		{"replicas left to the default", map[string]corev1alpha1.ComponentSpec{common.ComponentTiProxy: {}}, true},
		{"explicit replicas", map[string]corev1alpha1.ComponentSpec{common.ComponentTiProxy: {Replicas: replicas(2)}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, got := enabledTiProxy(tt.comps); got != tt.want {
				t.Errorf("enabledTiProxy() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildTiProxyGroup(t *testing.T) {
	in := withTiProxy(testInstance("8.5.2"), 3, testTiProxyVersion)
	comp := in.Spec.Components[common.ComponentTiProxy]
	comp.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("2"),
		corev1.ResourceMemory: resource.MustParse("2Gi"),
	}}
	comp.Parameters = &runtime.RawExtension{Raw: []byte(`{"config":"[log]\nlevel = \"warn\"\n"}`)}
	in.Spec.Components[common.ComponentTiProxy] = comp
	c := validationContext(t, in)

	g := buildTiProxyGroup(c, comp, "pingcap/tiproxy:v1.3.3")

	if g.Name != "orders" || g.Spec.Cluster.Name != "orders" {
		t.Errorf("group %q in cluster %q, want both named after the Instance", g.Name, g.Spec.Cluster.Name)
	}
	if g.Spec.Replicas == nil || *g.Spec.Replicas != 3 {
		t.Errorf("replicas = %v, want 3", g.Spec.Replicas)
	}
	spec := g.Spec.Template.Spec
	if spec.Version != testTiProxyVersion || spec.Image == nil || *spec.Image != "pingcap/tiproxy:v1.3.3" {
		t.Errorf("version %q image %v, want %s from pingcap/tiproxy", spec.Version, spec.Image, testTiProxyVersion)
	}
	if spec.Resources.CPU == nil || spec.Resources.CPU.String() != "2" || spec.Resources.Memory == nil || spec.Resources.Memory.String() != "2Gi" {
		t.Errorf("resources = %+v, want 2 CPU and 2Gi", spec.Resources)
	}
	if !strings.Contains(string(spec.Config), `level = "warn"`) {
		t.Errorf("config = %q, want the inline TOML", spec.Config)
	}
	if len(spec.Volumes) != 0 {
		t.Errorf("volumes = %v, want none: TiProxy is stateless", spec.Volumes)
	}
	if spec.Overlay == nil || spec.Overlay.Pod == nil || spec.Overlay.Pod.Labels[controller.ComponentLabel] != common.ComponentTiProxy {
		t.Errorf("overlay = %+v, want the runtime's pod labels for tiproxy", spec.Overlay)
	}
	if spec.Security != nil {
		t.Errorf("security = %+v, want none without TLS", spec.Security)
	}
}

func TestBuildTiProxyGroupDefaultReplicas(t *testing.T) {
	in := testInstance("8.5.2")
	in.Spec.Components[common.ComponentTiProxy] = corev1alpha1.ComponentSpec{Type: common.ComponentTiProxy}
	c := validationContext(t, in)

	g := buildTiProxyGroup(c, in.Spec.Components[common.ComponentTiProxy], "")
	if g.Spec.Replicas == nil || *g.Spec.Replicas != defaultTiProxyReplicas {
		t.Errorf("replicas = %v, want the default %d", g.Spec.Replicas, defaultTiProxyReplicas)
	}
}

func TestSyncTiProxyDeletesGroupOnceDisabled(t *testing.T) {
	in := withTiProxy(testInstance("8.5.2"), 0, "")
	c := validationContext(t, in, &tidbcorev1.TiProxyGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}})

	assertError(t, syncTiProxy(c, nil, nil), "")

	if found, err := c.Exists(&tidbcorev1.TiProxyGroup{}, "orders"); err != nil || found {
		t.Fatalf("TiProxy group still exists (err %v)", err)
	}
}

func TestSyncTiProxyWithoutGroupIsNoop(t *testing.T) {
	c := validationContext(t, testInstance("8.5.2"))
	assertError(t, syncTiProxy(c, nil, nil), "")
}

func TestEvaluateStatusWhileRemovingTiProxy(t *testing.T) {
	tiproxy := convergedGroup(common.ComponentTiProxy, 2)
	tiproxy.removing = true

	got := evaluateStatus(corev1alpha1.InstancePhaseReady, []groupRollout{convergedGroup(common.ComponentPD, 3), tiproxy})

	if got.Phase != corev1alpha1.InstancePhaseUpdating || !strings.Contains(got.Message, "tiproxy (removing)") {
		t.Fatalf("status = %s %q, want Updating while tiproxy is removed", got.Phase, got.Message)
	}
}

func TestStatusTiDBTiProxy(t *testing.T) {
	tiproxyGroup := func(ready int32) client.Object {
		g := &tidbcorev1.TiProxyGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}}
		g.Spec.Template.Spec.Version = testTiProxyVersion
		g.Spec.Replicas, g.Status.GroupStatus = convergedReplicas(2)
		g.Status.Version = testTiProxyVersion
		g.Status.ReadyReplicas = ready
		return g
	}
	tests := []struct {
		name        string
		previous    corev1alpha1.InstancePhase
		replicas    int32
		group       client.Object
		wantPhase   corev1alpha1.InstancePhase
		wantMessage string
		wantHost    string
	}{
		{"enabled before the group exists", "", 2, nil, corev1alpha1.InstancePhaseProvisioning, "tiproxy (0/2 ready)", ""},
		{"enabled on a ready instance before the group exists", corev1alpha1.InstancePhaseReady, 2, nil,
			corev1alpha1.InstancePhaseUpdating, "tiproxy (0/2 ready)", ""},
		{"enabled and starting", "", 2, tiproxyGroup(1), corev1alpha1.InstancePhaseProvisioning, "tiproxy (1/2 ready)", ""},
		{"enabled on a ready instance and starting", corev1alpha1.InstancePhaseReady, 2, tiproxyGroup(0),
			corev1alpha1.InstancePhaseUpdating, "tiproxy (0/2 ready)", ""},
		{"enabled and ready", "", 2, tiproxyGroup(2), corev1alpha1.InstancePhaseReady, "", "orders-tiproxy.db"},
		{"disabled and still running", corev1alpha1.InstancePhaseReady, 0, tiproxyGroup(2),
			corev1alpha1.InstancePhaseUpdating, "tiproxy (removing)", ""},
		{"disabled and gone", "", 0, nil, corev1alpha1.InstancePhaseReady, "", "orders-tidb.db"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append(convergedCoreGroups(), testProvider())
			if tt.group != nil {
				objs = append(objs, tt.group)
			}
			in := withTiProxy(testInstance("8.5.2"), tt.replicas, testTiProxyVersion)
			in.Status.Phase = tt.previous
			c := validationContext(t, in, objs...)

			got, err := StatusTiDB(c)
			assertError(t, err, "")
			if got.Phase != tt.wantPhase || !strings.Contains(got.Message, tt.wantMessage) {
				t.Errorf("status = %s %q, want %s containing %q", got.Phase, got.Message, tt.wantPhase, tt.wantMessage)
			}
			if got.ConnectionDetails.Host != tt.wantHost {
				t.Errorf("connection host = %q, want %q", got.ConnectionDetails.Host, tt.wantHost)
			}
		})
	}
}

func TestConnectionDetailsFollowTiProxy(t *testing.T) {
	tests := []struct {
		name     string
		replicas *int32
		wantHost string
		wantPort string
	}{
		{"without tiproxy", nil, "orders-tidb.db", "4000"},
		{"tiproxy turned off", ptrTo(int32(0)), "orders-tidb.db", "4000"},
		{"tiproxy on", ptrTo(int32(2)), "orders-tiproxy.db", "6000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := testInstance("8.5.2")
			if tt.replicas != nil {
				in = withTiProxy(in, *tt.replicas, "")
			}
			got := connectionDetails(validationContext(t, in))
			if got.Host != tt.wantHost || got.Port != tt.wantPort {
				t.Errorf("endpoint = %s:%s, want %s:%s", got.Host, got.Port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

func TestValidateTiDBTiProxyVersion(t *testing.T) {
	existing := func(version string) client.Object {
		g := &tidbcorev1.TiProxyGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}}
		g.Spec.Template.Spec.Version = version
		return g
	}
	tests := []struct {
		name     string
		replicas int32
		version  string
		existing client.Object
		wantErr  string
	}{
		{"own version line, not compared with tidb", 2, "v1.3.3", nil, ""},
		{"bundle version", 2, "", nil, ""},
		{"upgrade", 2, "v1.3.3", existing("v1.3.2"), ""},
		{"downgrade", 2, "v1.3.2", existing("v1.3.3"), "downgrading tiproxy from v1.3.3 to v1.3.2 is not supported"},
		{"invalid", 2, "latest", nil, `tiproxy version "latest" is not a valid semantic version`},
		{"disabled tiproxy is not checked", 0, "v1.3.2", existing("v1.3.3"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append([]client.Object{testProviderWithTiProxy()}, runningGroups("v8.5.2")...)
			if tt.existing != nil {
				objs = append(objs, tt.existing)
			}
			c := validationContext(t, withTiProxy(testInstance("8.5.2"), tt.replicas, tt.version), objs...)
			assertError(t, ValidateTiDB(c), tt.wantErr)
		})
	}
}

func withTiProxy(in *corev1alpha1.Instance, replicas int32, version string) *corev1alpha1.Instance {
	in.Spec.Components[common.ComponentTiProxy] = corev1alpha1.ComponentSpec{
		Type: common.ComponentTiProxy, Replicas: &replicas, Version: version,
	}
	return in
}

// testProviderWithTiProxy is testProvider with TiProxy in every version bundle.
func testProviderWithTiProxy() *corev1alpha1.Provider {
	p := testProvider()
	for i := range p.Spec.Versions {
		p.Spec.Versions[i].Components[common.ComponentTiProxy] = testTiProxyVersion
	}
	return p
}

func ptrTo[T any](v T) *T { return &v }

// tiproxyContext is a context for in with Secrets in the scheme and the given
// topology parameters (nil for none).
func tiproxyContext(t *testing.T, in *corev1alpha1.Instance, params any, objs ...client.Object) (*controller.Context, client.Client) {
	t.Helper()
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		in.Spec.Topology = &corev1alpha1.TopologySpec{Type: "cluster", Parameters: &runtime.RawExtension{Raw: raw}}
	}
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, tidbcorev1.Install, corev1.AddToScheme} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return controller.NewContext(context.Background(), cl, in, common.ProviderName), cl
}

func gateNames(gates []metav1alpha1.FeatureGate) []metav1alpha1.Feature {
	names := make([]metav1alpha1.Feature, 0, len(gates))
	for _, g := range gates {
		names = append(names, g.Name)
	}
	return names
}

func clusterWithGates(signingSecret string, gates ...metav1alpha1.Feature) *tidbcorev1.Cluster {
	cl := testCluster()
	for _, g := range gates {
		cl.Spec.FeatureGates = append(cl.Spec.FeatureGates, metav1alpha1.FeatureGate{Name: g})
	}
	if signingSecret != "" {
		cl.Spec.Security = &tidbcorev1.ClusterSecurity{SessionTokenSigningCertKeyPair: &corev1.LocalObjectReference{Name: signingSecret}}
	}
	return cl
}

func tidbGroupWithFeatures(features ...metav1alpha1.Feature) *tidbcorev1.TiDBGroup {
	g := &tidbcorev1.TiDBGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}}
	g.Spec.Template.Spec.Version = testVersion
	g.Spec.Features = features
	return g
}

func tidbInstanceWithFeatures(name string, features ...metav1alpha1.Feature) *tidbcorev1.TiDB {
	db := &tidbcorev1.TiDB{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: map[string]string{
		tidbcorev1.LabelKeyCluster: "orders",
		tidbcorev1.LabelKeyGroup:   "orders",
	}}}
	db.Spec.Features = features
	return db
}

func signingSecret() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "orders-session-token-signing-secret", Namespace: testNamespace}}
}

func TestBuildClusterSessionTokenSigning(t *testing.T) {
	const fm, sts = metav1alpha1.FeatureModification, metav1alpha1.SessionTokenSigning
	const secret = "orders-session-token-signing-secret"
	tests := []struct {
		name        string
		tiproxy     *int32
		objs        []client.Object
		wantGates   []metav1alpha1.Feature
		wantSigning string
	}{
		{name: "new cluster without tiproxy"},
		{name: "new cluster with tiproxy", tiproxy: ptrTo(int32(2)),
			wantGates: []metav1alpha1.Feature{fm, sts}, wantSigning: secret},
		{name: "new cluster with tiproxy at 0 replicas", tiproxy: ptrTo(int32(0))},
		{name: "existing cluster without tiproxy", objs: []client.Object{testCluster()}},
		{name: "tiproxy turned on: FeatureModification first", tiproxy: ptrTo(int32(2)),
			objs:      []client.Object{testCluster(), signingSecret(), tidbGroupWithFeatures(), tidbInstanceWithFeatures("orders-a")},
			wantGates: []metav1alpha1.Feature{fm}},
		{name: "waits for the tidb group to carry FeatureModification", tiproxy: ptrTo(int32(2)),
			objs:      []client.Object{clusterWithGates("", fm), signingSecret(), tidbGroupWithFeatures()},
			wantGates: []metav1alpha1.Feature{fm}},
		{name: "waits for every tidb instance to carry FeatureModification", tiproxy: ptrTo(int32(2)),
			objs: []client.Object{clusterWithGates("", fm), signingSecret(), tidbGroupWithFeatures(fm),
				tidbInstanceWithFeatures("orders-a", fm), tidbInstanceWithFeatures("orders-b")},
			wantGates: []metav1alpha1.Feature{fm}},
		{name: "waits for the signing secret", tiproxy: ptrTo(int32(2)),
			objs:      []client.Object{clusterWithGates("", fm), tidbGroupWithFeatures(fm), tidbInstanceWithFeatures("orders-a", fm)},
			wantGates: []metav1alpha1.Feature{fm}},
		{name: "then SessionTokenSigning", tiproxy: ptrTo(int32(2)),
			objs: []client.Object{clusterWithGates("", fm), signingSecret(), tidbGroupWithFeatures(fm),
				tidbInstanceWithFeatures("orders-a", fm), tidbInstanceWithFeatures("orders-b", fm)},
			wantGates: []metav1alpha1.Feature{fm, sts}, wantSigning: secret},
		{name: "gates stay after tiproxy is turned off", tiproxy: ptrTo(int32(0)),
			objs:      []client.Object{clusterWithGates(secret, fm, sts)},
			wantGates: []metav1alpha1.Feature{fm, sts}, wantSigning: secret},
		{name: "FeatureModification stays after tiproxy is turned off half way",
			objs: []client.Object{clusterWithGates("", fm)}, wantGates: []metav1alpha1.Feature{fm}},
		{name: "a signing secret set by someone else is kept", tiproxy: ptrTo(int32(2)),
			objs:      []client.Object{clusterWithGates("custom", sts)},
			wantGates: []metav1alpha1.Feature{fm, sts}, wantSigning: "custom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := testInstance("8.5.2")
			if tt.tiproxy != nil {
				in = withTiProxy(in, *tt.tiproxy, "")
			}
			c, _ := tiproxyContext(t, in, nil, tt.objs...)
			built, err := buildCluster(c)
			assertError(t, err, "")
			if got := gateNames(built.Spec.FeatureGates); !slices.Equal(got, tt.wantGates) {
				t.Errorf("feature gates = %v, want %v", got, tt.wantGates)
			}
			var signing string
			if sec := built.Spec.Security; sec != nil && sec.SessionTokenSigningCertKeyPair != nil {
				signing = sec.SessionTokenSigningCertKeyPair.Name
			}
			if signing != tt.wantSigning {
				t.Errorf("session token signing secret = %q, want %q", signing, tt.wantSigning)
			}
		})
	}
}

func TestEnsureSessionTokenSigningSecret(t *testing.T) {
	tests := []struct {
		name    string
		tiproxy int32
		cluster *tidbcorev1.Cluster
		want    []string
	}{
		{"without tiproxy", 0, testCluster(), nil},
		{"with tiproxy and no TLS", 2, testCluster(), []string{"orders-session-token-signing-secret", "orders-tls-ca"}},
		{"kept while the cluster uses it", 0, clusterWithGates("orders-session-token-signing-secret",
			metav1alpha1.FeatureModification, metav1alpha1.SessionTokenSigning),
			[]string{"orders-session-token-signing-secret", "orders-tls-ca"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cl := tiproxyContext(t, withTiProxy(testInstance("8.5.2"), tt.tiproxy, ""), nil)
			assertError(t, ensureTLSSecrets(c, tt.cluster), "")

			list := &corev1.SecretList{}
			if err := cl.List(context.Background(), list); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, s := range list.Items {
				got = append(got, s.Name)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("secrets = %v, want %v", got, tt.want)
			}
			if len(got) == 0 {
				return
			}
			secret := mustSecret(t, cl, "orders-session-token-signing-secret")
			if len(secret.Data["tls.key"]) == 0 {
				t.Error("signing secret has no tls.key")
			}
			if cert := parseCert(t, secret.Data["tls.crt"]); cert.Subject.CommonName != "orders-tidb-session-token" {
				t.Errorf("signing certificate CN = %q", cert.Subject.CommonName)
			}
		})
	}
}
