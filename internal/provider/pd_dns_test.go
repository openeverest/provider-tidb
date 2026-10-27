package provider

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

func pdContext(t *testing.T, objs ...client.Object) *controller.Context {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := tidbcorev1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return controller.NewContext(context.Background(), cl, testInstance(testVersion), common.ProviderName)
}

func TestNewPDGroupWaitsForItsDNSName(t *testing.T) {
	c := pdContext(t)
	comp := testInstance(testVersion).Spec.Components[common.ComponentPD]
	comp.SchedulingPolicy = &commonv1alpha1.SchedulingPolicy{NodeSelector: map[string]string{"disk": "ssd"}}
	group := buildPDGroup(c, comp, "pingcap/pd:v8.5.2")

	if err := addPDDNSWait(c, group, "pingcap/pd:v8.5.2"); err != nil {
		t.Fatal(err)
	}
	spec := group.Spec.Template.Spec.Overlay.Pod.Spec
	if spec.NodeSelector["disk"] != "ssd" {
		t.Errorf("scheduling policy lost: %+v", spec.NodeSelector)
	}
	if len(spec.InitContainers) != 1 {
		t.Fatalf("init containers = %+v", spec.InitContainers)
	}
	ic := spec.InitContainers[0]
	if ic.Name != pdDNSWaitContainer || ic.Image != "pingcap/pd:v8.5.2" {
		t.Errorf("init container = %s %s", ic.Name, ic.Image)
	}
	if len(ic.Env) != 1 || ic.Env[0].Value != "orders-pd-peer.db" {
		t.Errorf("env = %+v, want PEER_DOMAIN=orders-pd-peer.db", ic.Env)
	}
	for _, r := range []string{"cpu", "memory"} {
		req, lim := ic.Resources.Requests[corev1.ResourceName(r)], ic.Resources.Limits[corev1.ResourceName(r)]
		if req.IsZero() || !req.Equal(lim) {
			t.Errorf("%s request %v != limit %v: the pod would lose Guaranteed QoS", r, req.String(), lim.String())
		}
	}
}

func TestExistingPDGroupWithoutDNSWaitIsLeftAlone(t *testing.T) {
	in := testInstance(testVersion)
	existing := &tidbcorev1.PDGroup{}
	existing.Name, existing.Namespace = in.Name, testNamespace
	c := pdContext(t, existing)
	group := buildPDGroup(c, in.Spec.Components[common.ComponentPD], "pingcap/pd:v8.5.2")

	if err := addPDDNSWait(c, group, "pingcap/pd:v8.5.2"); err != nil {
		t.Fatal(err)
	}
	if hasPDDNSWait(group) {
		t.Error("an existing cluster got the init container, which would roll its PD pods")
	}
}

func TestPDGroupKeepsItsDNSWait(t *testing.T) {
	in := testInstance(testVersion)
	c := pdContext(t)
	existing := buildPDGroup(c, in.Spec.Components[common.ComponentPD], "pingcap/pd:v8.5.2")
	if err := addPDDNSWait(c, existing, "pingcap/pd:v8.5.2"); err != nil {
		t.Fatal(err)
	}
	existing.Namespace = testNamespace
	c = pdContext(t, existing)

	group := buildPDGroup(c, in.Spec.Components[common.ComponentPD], "pingcap/pd:v8.5.3")
	if err := addPDDNSWait(c, group, "pingcap/pd:v8.5.3"); err != nil {
		t.Fatal(err)
	}
	if !hasPDDNSWait(group) {
		t.Fatal("the init container was dropped from a group that had it")
	}
	if img := group.Spec.Template.Spec.Overlay.Pod.Spec.InitContainers[0].Image; img != "pingcap/pd:v8.5.3" {
		t.Errorf("init container image = %s, want it to follow the PD version", img)
	}
}
