package provider

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition"
)

func namesTLS(mode definition.TLSMode, names ...string) *definition.TLSParameters {
	return &definition.TLSParameters{Mode: mode, AdditionalNames: names}
}

func clusterWithNames(names string) *tidbcorev1.Cluster {
	cl := testCluster()
	cl.Spec.TLSCluster = &tidbcorev1.TLSCluster{Enabled: true}
	cl.Annotations = map[string]string{tlsSourceAnnotation: tlsSourceProvider, tlsNamesAnnotation: names}
	return cl
}

func runningTiDBGroup() *tidbcorev1.TiDBGroup {
	return &tidbcorev1.TiDBGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}}
}

func TestAppliedAdditionalNames(t *testing.T) {
	desired := namesTLS(definition.TLSModePrefer, "b.example.com", "a.example.com", "a.example.com")
	tests := []struct {
		name        string
		running     bool
		recorded    string
		approve     string
		want        []string
		wantPending bool
	}{
		{name: "new instance takes them", want: []string{"a.example.com", "b.example.com"}},
		{name: "running instance waits for approval", running: true, recorded: "old.example.com",
			want: []string{"old.example.com"}, wantPending: true},
		{name: "running instance applies them once approved", running: true, recorded: "old.example.com",
			approve: "tls-names-" + namesHash([]string{"a.example.com", "b.example.com"}),
			want:    []string{"a.example.com", "b.example.com"}},
		{name: "unchanged names need no approval", running: true, recorded: "a.example.com,b.example.com",
			want: []string{"a.example.com", "b.example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []client.Object
			if tt.running {
				objs = append(objs, runningTiDBGroup())
			}
			c, _ := tlsContext(t, desired, objs...)
			if tt.approve != "" {
				c.Instance().Spec.Maintenance = &corev1alpha1.MaintenanceSpec{Approved: tt.approve}
			}
			got, err := appliedAdditionalNames(c, clusterWithNames(tt.recorded), tlsParameters(c))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("names = %v, want %v", got, tt.want)
			}
			pending := c.GetPendingMaintenance()
			if (len(pending) > 0) != tt.wantPending {
				t.Fatalf("pending maintenance = %v, want pending %v", pending, tt.wantPending)
			}
			if tt.wantPending && (pending[0].Severity != controller.MaintenanceRollingRestart ||
				!strings.HasPrefix(pending[0].ApprovalToken, "tls-names-")) {
				t.Errorf("pending maintenance = %+v, want a rolling restart with a tls-names token", pending[0])
			}
		})
	}
}

// The provider reissues TiDB's and TiProxy's MySQL certificates for new
// additional names and keeps every other certificate.
func TestEnsureTLSSecretsReissuesForNames(t *testing.T) {
	c, cl := tlsContext(t, tlsMode(definition.TLSModePrefer))
	owner := testCluster()
	if err := ensureTLSSecrets(c, owner, tlsSourceProvider, nil); err != nil {
		t.Fatal(err)
	}
	pd := mustSecret(t, cl, "orders-pd-cluster-secret").Data[tlsKeyCert]
	server := mustSecret(t, cl, mysqlServerTLSSecretName("orders")).Data[tlsKeyCert]
	if slices.Contains(parseCert(t, server).DNSNames, "tidb.example.com") {
		t.Fatal("certificate has the additional name before it is asked for")
	}

	if err := ensureTLSSecrets(c, owner, tlsSourceProvider, []string{"tidb.example.com"}); err != nil {
		t.Fatal(err)
	}
	reissued := mustSecret(t, cl, mysqlServerTLSSecretName("orders")).Data[tlsKeyCert]
	if !slices.Contains(parseCert(t, reissued).DNSNames, "tidb.example.com") {
		t.Errorf("TiDB's certificate names = %v, want tidb.example.com", parseCert(t, reissued).DNSNames)
	}
	if got := mustSecret(t, cl, "orders-pd-cluster-secret").Data[tlsKeyCert]; string(got) != string(pd) {
		t.Error("PD's certificate was reissued")
	}
}

func TestAddTLSNamesRestart(t *testing.T) {
	issued := func(t *testing.T, names []string) []client.Object {
		c, cl := tlsContext(t, tlsMode(definition.TLSModePrefer))
		if err := ensureTLSSecrets(c, testCluster(), tlsSourceProvider, names); err != nil {
			t.Fatal(err)
		}
		return []client.Object{mustSecret(t, cl, mysqlServerTLSSecretName("orders"))}
	}
	names := []string{"tidb.example.com"}
	tests := []struct {
		name        string
		issuedFor   []string
		applied     []string
		want        string
		wantPending bool
	}{
		{name: "no additional names, no annotation", issuedFor: nil, applied: nil},
		{name: "certificate carries the names", issuedFor: names, applied: names, want: namesHash(names)},
		{name: "certificate not reissued yet", issuedFor: nil, applied: names, wantPending: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := tlsContext(t, namesTLS(definition.TLSModePrefer, tt.applied...), issued(t, tt.issuedFor)...)
			group := buildTiDBGroup(c, c.Instance().Spec.Components["tidb"], "")
			pending, err := addTLSNamesRestart(c, group, tt.applied)
			if err != nil {
				t.Fatal(err)
			}
			if pending != tt.wantPending {
				t.Errorf("pending = %v, want %v", pending, tt.wantPending)
			}
			if got := group.Spec.Template.Spec.Overlay.Pod.Annotations[tidbTLSNamesAnnotation]; got != tt.want {
				t.Errorf("annotation = %q, want %q", got, tt.want)
			}
		})
	}
}

func servingTiDBGroup(rolling bool) *tidbcorev1.TiDBGroup {
	g := runningTiDBGroup()
	g.Spec.Replicas = ptr.To[int32](2)
	g.Spec.Template.Spec.Security = &tidbcorev1.TiDBSecurity{TLS: &tidbcorev1.TiDBTLS{MySQL: &tidbcorev1.TLS{Enabled: true}}}
	g.Status.Replicas, g.Status.ReadyReplicas, g.Status.UpdatedReplicas = 2, 2, 2
	g.Status.CurrentRevision, g.Status.UpdateRevision = "r1", "r1"
	if rolling {
		g.Status.UpdateRevision = "r2"
	}
	return g
}

func TestSyncRequireSecureTransport(t *testing.T) {
	tests := []struct {
		name      string
		mode      definition.TLSMode
		set       bool
		rolling   bool
		failSQL   bool
		wantCall  string
		wantSet   bool
		wantError bool
	}{
		{name: "requireTLS turns it on", mode: definition.TLSModeRequire, wantCall: "on", wantSet: true},
		{name: "requireTLS waits for TiDB to roll out", mode: definition.TLSModeRequire, rolling: true},
		{name: "requireTLS already on", mode: definition.TLSModeRequire, set: true, wantSet: true},
		{name: "preferTLS turns it off", mode: definition.TLSModePrefer, set: true, wantCall: "off"},
		{name: "preferTLS leaves an administrator's setting alone", mode: definition.TLSModePrefer},
		{name: "failed SQL retries", mode: definition.TLSModeRequire, failSQL: true, wantCall: "on", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var call string
			orig := setRequireSecureTransport
			t.Cleanup(func() { setRequireSecureTransport = orig })
			setRequireSecureTransport = func(_ context.Context, ep tidbEndpoint, on bool) error {
				call = map[bool]string{true: "on", false: "off"}[on]
				if ep.serverName != "orders-tidb."+testNamespace+".svc" || string(ep.ca) != "ca" || ep.password != "secret" {
					t.Errorf("endpoint = %+v", ep)
				}
				if tt.failSQL {
					return errors.New("connection refused")
				}
				return nil
			}
			cluster := testCluster()
			if tt.set {
				cluster.Annotations = map[string]string{requireSecureTransportAnnotation: "true"}
			}
			c, cl := tlsContext(t, tlsMode(tt.mode), cluster, servingTiDBGroup(tt.rolling),
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: mysqlServerTLSSecretName("orders"), Namespace: testNamespace},
					Data: map[string][]byte{tlsKeyCA: []byte("ca")}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: rootSecretName("orders"), Namespace: testNamespace},
					Data: map[string][]byte{rootPasswordKey: []byte("secret")}})
			err := syncRequireSecureTransport(c, cluster)
			if (err != nil) != tt.wantError {
				t.Fatalf("err = %v, want error %v", err, tt.wantError)
			}
			if call != tt.wantCall {
				t.Errorf("SQL call = %q, want %q", call, tt.wantCall)
			}
			got := &tidbcorev1.Cluster{}
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(cluster), got); err != nil {
				t.Fatal(err)
			}
			if set := got.Annotations[requireSecureTransportAnnotation] == "true"; set != tt.wantSet {
				t.Errorf("annotation set = %v, want %v", set, tt.wantSet)
			}
		})
	}
}

func TestValidateTLSParameters(t *testing.T) {
	legacy := func() *corev1alpha1.Instance {
		in := tlsInstance(t, nil)
		in.Spec.Topology.Parameters = &runtime.RawExtension{Raw: []byte(`{"tls":{"cluster":true,"client":true}}`)}
		return in
	}
	tests := []struct {
		name      string
		tls       *definition.TLSParameters
		instance  *corev1alpha1.Instance
		old       *corev1alpha1.Instance
		wantField string
	}{
		{name: "preferTLS", tls: tlsMode(definition.TLSModePrefer)},
		{name: "unknown mode", tls: tlsMode("on"), wantField: "spec.parameters.tls.mode"},
		{name: "issuer needs TLS", tls: &definition.TLSParameters{Issuer: &definition.IssuerReference{Name: "corp"}},
			wantField: "spec.parameters.tls.issuer"},
		{name: "issuer needs a name", tls: issuerTLS("", definition.IssuerKindClusterIssuer, ""),
			wantField: "spec.parameters.tls.issuer.name"},
		{name: "unknown cert-manager kind", tls: issuerTLS("corp", "Vault", ""),
			wantField: "spec.parameters.tls.issuer.kind"},
		{name: "external issuer kind", tls: issuerTLS("pca", "AWSPCAClusterIssuer", "awspca.cert-manager.io")},
		{name: "wildcard name", tls: namesTLS(definition.TLSModePrefer, "*.db.example.com")},
		{name: "IP address", tls: namesTLS(definition.TLSModePrefer, "10.0.0.1"),
			wantField: "spec.parameters.tls.additionalNames[0]"},
		{name: "invalid DNS name", tls: namesTLS(definition.TLSModePrefer, "Not_A_Name"),
			wantField: "spec.parameters.tls.additionalNames[0]"},
		{name: "names need TLS", tls: namesTLS(definition.TLSModeDisabled, "db.example.com"),
			wantField: "spec.parameters.tls.additionalNames"},
		{name: "legacy fields rejected", instance: legacy(), wantField: "spec.topology.parameters.tls"},
		{name: "preferTLS to requireTLS", tls: tlsMode(definition.TLSModeRequire),
			old: tlsInstance(t, tlsMode(definition.TLSModePrefer))},
		{name: "TLS can't be turned off", tls: tlsMode(definition.TLSModeDisabled),
			old: tlsInstance(t, tlsMode(definition.TLSModePrefer)), wantField: "spec.parameters.tls.mode"},
		{name: "TLS can't be turned on", tls: tlsMode(definition.TLSModePrefer),
			old: tlsInstance(t, nil), wantField: "spec.parameters.tls.mode"},
		{name: "issuer can't change", tls: issuerTLS("corp", definition.IssuerKindClusterIssuer, ""),
			old: tlsInstance(t, tlsMode(definition.TLSModePrefer)), wantField: "spec.parameters.tls.issuer"},
		{name: "migrating from the legacy fields", tls: tlsMode(definition.TLSModePrefer), old: legacy()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.instance
			if in == nil {
				in = tlsInstance(t, tt.tls)
			}
			errs := validateTLSParameters(in, tt.old)
			if tt.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("errors = %v, want none", errs)
				}
				return
			}
			if len(errs) != 1 || errs[0].Field != tt.wantField {
				t.Fatalf("errors = %v, want one on %s", errs, tt.wantField)
			}
		})
	}
}
