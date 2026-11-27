package provider

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

func withService(in *corev1alpha1.Instance, component string, svc *corev1alpha1.Service) *corev1alpha1.Instance {
	comp := in.Spec.Components[component]
	comp.Service = svc
	in.Spec.Components[component] = comp
	return in
}

// syncServices runs syncExternalServices and reports whether it waits for a
// Service to be deleted.
func syncServices(t *testing.T, c *controller.Context) bool {
	t.Helper()
	pending, err := syncExternalServices(c)
	assertError(t, err, "")
	return pending
}

func TestSyncExternalServices(t *testing.T) {
	in := withService(testInstance("8.5.2"), common.ComponentTiDB, &corev1alpha1.Service{
		ServiceType: corev1.ServiceTypeLoadBalancer,
		Annotations: map[string]string{"service.beta.kubernetes.io/linode-loadbalancer-throttle": "5"},
		LoadBalancerService: &corev1alpha1.LoadBalancerService{
			SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8"},
		},
	})
	c, _ := tiproxyContext(t, in, nil)

	syncServices(t, c)

	svc := &corev1.Service{}
	if err := c.Get(svc, "orders-tidb-external"); err != nil {
		t.Fatalf("external TiDB service: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("type = %s, want LoadBalancer", svc.Spec.Type)
	}
	if svc.Annotations["service.beta.kubernetes.io/linode-loadbalancer-throttle"] != "5" {
		t.Errorf("annotations = %v, want the component's", svc.Annotations)
	}
	if got := svc.Spec.LoadBalancerSourceRanges; len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Errorf("source ranges = %v, want [10.0.0.0/8]", got)
	}
	wantPort := corev1.ServicePort{Name: "mysql-client", Protocol: corev1.ProtocolTCP, Port: 4000, TargetPort: intstr.FromString("mysql-client")}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0] != wantPort {
		t.Errorf("ports = %+v, want only %+v", svc.Spec.Ports, wantPort)
	}
	if svc.Spec.Selector[tidbcorev1.LabelKeyComponent] != "tidb" || svc.Spec.Selector[tidbcorev1.LabelKeyGroup] != "orders" ||
		svc.Spec.Selector[tidbcorev1.LabelKeyCluster] != "orders" {
		t.Errorf("selector = %v, want the TiDB pods of orders", svc.Spec.Selector)
	}
	if found, _ := c.Exists(&corev1.Service{}, "orders-tiproxy-external"); found {
		t.Error("TiProxy is not exposed, yet has an external service")
	}

	// LoadBalancer to NodePort: deleted first, so the cloud load balancer is
	// released, then created again as NodePort.
	withService(in, common.ComponentTiDB, &corev1alpha1.Service{ServiceType: corev1.ServiceTypeNodePort})
	if !syncServices(t, c) {
		t.Error("switching to NodePort did not wait for the LoadBalancer Service to be deleted")
	}
	if found, err := c.Exists(&corev1.Service{}, "orders-tidb-external"); err != nil || found {
		t.Fatalf("LoadBalancer Service not deleted before NodePort (err %v)", err)
	}
	if syncServices(t, c) {
		t.Error("still pending once the LoadBalancer Service is gone")
	}
	svc = &corev1.Service{}
	if err := c.Get(svc, "orders-tidb-external"); err != nil || svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Fatalf("external TiDB service = %s (err %v), want NodePort", svc.Spec.Type, err)
	}

	// Back to in-cluster only: the external service goes.
	withService(in, common.ComponentTiDB, &corev1alpha1.Service{ServiceType: corev1.ServiceTypeClusterIP})
	syncServices(t, c)
	if found, err := c.Exists(&corev1.Service{}, "orders-tidb-external"); err != nil || found {
		t.Fatalf("external TiDB service still exists (err %v)", err)
	}
}

func TestSyncExternalServicesTiProxy(t *testing.T) {
	in := withService(withTiProxy(testInstance("8.5.2"), 2, ""), common.ComponentTiProxy,
		&corev1alpha1.Service{ServiceType: corev1.ServiceTypeNodePort})
	c, _ := tiproxyContext(t, in, nil)

	syncServices(t, c)
	svc := &corev1.Service{}
	if err := c.Get(svc, "orders-tiproxy-external"); err != nil {
		t.Fatalf("external TiProxy service: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort || svc.Spec.Ports[0].Port != 6000 ||
		svc.Spec.Selector[tidbcorev1.LabelKeyComponent] != "tiproxy" {
		t.Errorf("service = %s %+v %v, want a NodePort on TiProxy's port 6000", svc.Spec.Type, svc.Spec.Ports, svc.Spec.Selector)
	}

	// Turning TiProxy off removes its external service with it.
	comp := in.Spec.Components[common.ComponentTiProxy]
	comp.Replicas = ptrTo(int32(0))
	in.Spec.Components[common.ComponentTiProxy] = comp
	syncServices(t, c)
	if found, err := c.Exists(&corev1.Service{}, "orders-tiproxy-external"); err != nil || found {
		t.Fatalf("external TiProxy service still exists (err %v)", err)
	}
}

func externalService(name string, typ corev1.ServiceType, port, nodePort int32, ingress ...corev1.LoadBalancerIngress) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: corev1.ServiceSpec{
			Type:  typ,
			Ports: []corev1.ServicePort{{Name: "mysql-client", Port: port, NodePort: nodePort}},
		},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: ingress}},
	}
}

func TestConnectionDetailsExternal(t *testing.T) {
	lb := &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer}
	nodePort := &corev1alpha1.Service{ServiceType: corev1.ServiceTypeNodePort}
	tests := []struct {
		name         string
		tiproxy      bool
		component    string
		service      *corev1alpha1.Service
		existing     *corev1.Service
		wantHost     string
		wantPort     string
		wantInternal string
		wantNodePort string
	}{
		{
			name: "load balancer address assigned", component: common.ComponentTiDB, service: lb,
			existing: externalService("orders-tidb-external", corev1.ServiceTypeLoadBalancer, 4000, 31000, corev1.LoadBalancerIngress{IP: "203.0.113.7"}),
			wantHost: "203.0.113.7", wantPort: "4000", wantInternal: "orders-tidb.db",
		},
		{
			name: "load balancer hostname preferred", component: common.ComponentTiDB, service: lb,
			existing: externalService("orders-tidb-external", corev1.ServiceTypeLoadBalancer, 4000, 31000,
				corev1.LoadBalancerIngress{IP: "203.0.113.7"}, corev1.LoadBalancerIngress{Hostname: "lb.example.com"}),
			wantHost: "lb.example.com", wantPort: "4000", wantInternal: "orders-tidb.db",
		},
		{
			name: "load balancer address pending", component: common.ComponentTiDB, service: lb,
			existing: externalService("orders-tidb-external", corev1.ServiceTypeLoadBalancer, 4000, 31000),
			wantHost: "orders-tidb.db", wantPort: "4000", wantInternal: "orders-tidb.db",
		},
		{
			name: "external service not created yet", component: common.ComponentTiDB, service: lb,
			wantHost: "orders-tidb.db", wantPort: "4000",
		},
		{
			name: "node port", component: common.ComponentTiDB, service: nodePort,
			existing: externalService("orders-tidb-external", corev1.ServiceTypeNodePort, 4000, 31234),
			wantHost: "orders-tidb.db", wantPort: "4000", wantInternal: "orders-tidb.db", wantNodePort: "31234",
		},
		{
			name: "tiproxy exposed", tiproxy: true, component: common.ComponentTiProxy, service: lb,
			existing: externalService("orders-tiproxy-external", corev1.ServiceTypeLoadBalancer, 6000, 31000, corev1.LoadBalancerIngress{IP: "203.0.113.8"}),
			wantHost: "203.0.113.8", wantPort: "6000", wantInternal: "orders-tiproxy.db",
		},
		{
			// Clients go through TiProxy, so exposing TiDB alone leaves them in-cluster.
			name: "tidb exposed behind tiproxy", tiproxy: true, component: common.ComponentTiDB, service: lb,
			existing: externalService("orders-tidb-external", corev1.ServiceTypeLoadBalancer, 4000, 31000, corev1.LoadBalancerIngress{IP: "203.0.113.7"}),
			wantHost: "orders-tiproxy.db", wantPort: "6000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := testInstance("8.5.2")
			if tt.tiproxy {
				in = withTiProxy(in, 2, "")
			}
			withService(in, tt.component, tt.service)
			var objs []client.Object
			if tt.existing != nil {
				objs = append(objs, tt.existing)
			}
			c, _ := tiproxyContext(t, in, nil, objs...)

			got := connectionDetails(c)
			if got.Host != tt.wantHost || got.Port != tt.wantPort {
				t.Errorf("endpoint = %s:%s, want %s:%s", got.Host, got.Port, tt.wantHost, tt.wantPort)
			}
			if got.AdditionalProperties[connKeyInternalHost] != tt.wantInternal {
				t.Errorf("internal_host = %q, want %q", got.AdditionalProperties[connKeyInternalHost], tt.wantInternal)
			}
			if got.AdditionalProperties[connKeyNodePort] != tt.wantNodePort {
				t.Errorf("node_port = %q, want %q", got.AdditionalProperties[connKeyNodePort], tt.wantNodePort)
			}
			if !strings.Contains(got.URI, "@"+tt.wantHost+":"+tt.wantPort+"/") {
				t.Errorf("uri = %q, want it to point at %s:%s", got.URI, tt.wantHost, tt.wantPort)
			}
		})
	}
}

func TestValidateServices(t *testing.T) {
	t.Run("unsupported type", func(t *testing.T) {
		in := withService(testInstance("8.5.2"), common.ComponentTiDB, &corev1alpha1.Service{ServiceType: corev1.ServiceTypeExternalName})
		errs, _ := validateServices(in.Spec.Components)
		if len(errs) != 1 || errs[0].Field != "spec.components[tidb].service.serviceType" {
			t.Fatalf("errors = %v, want one on spec.components[tidb].service.serviceType", errs)
		}
	})
	for _, typ := range supportedServiceTypes {
		in := withService(withTiProxy(testInstance("8.5.2"), 1, ""), common.ComponentTiProxy, &corev1alpha1.Service{ServiceType: typ})
		if errs, _ := validateServices(in.Spec.Components); len(errs) != 0 {
			t.Errorf("%s: unexpected errors %v", typ, errs)
		}
	}
	t.Run("tidb exposed behind tiproxy warns", func(t *testing.T) {
		in := withService(withTiProxy(testInstance("8.5.2"), 1, ""), common.ComponentTiDB, &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer})
		if _, warnings := validateServices(in.Spec.Components); len(warnings) != 1 {
			t.Fatalf("warnings = %v, want one", warnings)
		}
		withService(in, common.ComponentTiProxy, &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer})
		if _, warnings := validateServices(in.Spec.Components); len(warnings) != 0 {
			t.Fatalf("warnings = %v, want none once TiProxy is exposed too", warnings)
		}
	})
}
