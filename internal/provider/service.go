package provider

import (
	"fmt"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// Connection details keys published while the SQL endpoint is exposed outside
// the cluster: the in-cluster endpoint, and the node port of a NodePort Service.
const (
	connKeyInternalHost = "internal_host"
	connKeyInternalPort = "internal_port"
	connKeyNodePort     = "node_port"
)

// supportedServiceTypes are the service.serviceType values the SQL components
// accept. ExternalName does not expose pods, so it is not offered.
var supportedServiceTypes = []corev1.ServiceType{
	corev1.ServiceTypeClusterIP,
	corev1.ServiceTypeNodePort,
	corev1.ServiceTypeLoadBalancer,
}

// exposedComponents are the components that serve SQL clients and so can be
// exposed outside the cluster through component.service.
var exposedComponents = []string{common.ComponentTiDB, common.ComponentTiProxy}

// sqlEndpoint is where a SQL component listens: its operator-managed
// in-cluster Service and the MySQL client port.
type sqlEndpoint struct {
	component string
	label     string
	portName  string
	port      int32
}

func endpointOf(component string) sqlEndpoint {
	if component == common.ComponentTiProxy {
		return sqlEndpoint{component, tidbcorev1.LabelValComponentTiProxy, tidbcorev1.TiProxyPortNameClient, tidbcorev1.DefaultTiProxyPortClient}
	}
	return sqlEndpoint{component, tidbcorev1.LabelValComponentTiDB, tidbcorev1.TiDBPortNameClient, tidbcorev1.DefaultTiDBPortClient}
}

// clientEndpoint is the SQL component clients connect to: TiProxy while it
// runs, otherwise TiDB.
func clientEndpoint(comps map[string]corev1alpha1.ComponentSpec) sqlEndpoint {
	if _, ok := enabledTiProxy(comps); ok {
		return endpointOf(common.ComponentTiProxy)
	}
	return endpointOf(common.ComponentTiDB)
}

// internalHost is the operator's ClusterIP Service in front of the component.
func (e sqlEndpoint) internalHost(c *controller.Context) string {
	return fmt.Sprintf("%s-%s.%s", c.Name(), e.component, c.Namespace())
}

// externalServiceName is the Service the provider manages to expose a SQL
// component outside the cluster. TiDB Operator v2 always makes its own
// "<group>-tidb" and "<group>-tiproxy" Services ClusterIP and has no field to
// change that, so the provider adds one next to it.
func externalServiceName(instance, component string) string {
	return fmt.Sprintf("%s-%s-external", instance, component)
}

// exposed reports whether the service asks for access from outside the
// cluster. Validate admits only LoadBalancer and NodePort besides ClusterIP.
func exposed(svc *corev1alpha1.Service) bool {
	return svc != nil && svc.ServiceType != "" && svc.ServiceType != corev1.ServiceTypeClusterIP
}

// syncExternalServices creates or updates the external Service of each SQL
// component that runs and asks for LoadBalancer or NodePort access, and
// deletes it otherwise: back to ClusterIP, or the component (TiProxy) turned off.
func syncExternalServices(c *controller.Context) error {
	comps := c.Instance().Spec.Components
	for _, component := range exposedComponents {
		comp, running := comps[component], true
		if component == common.ComponentTiProxy {
			comp, running = enabledTiProxy(comps)
		}
		if running && exposed(comp.Service) {
			if err := c.Apply(buildExternalService(c, endpointOf(component), comp.Service)); err != nil {
				return fmt.Errorf("apply %s external service: %w", component, err)
			}
			continue
		}
		svc := &corev1.Service{}
		found, err := c.Exists(svc, externalServiceName(c.Name(), component))
		if err != nil {
			return fmt.Errorf("get %s external service: %w", component, err)
		}
		if found {
			if err := c.Delete(svc); err != nil {
				return fmt.Errorf("delete %s external service: %w", component, err)
			}
		}
	}
	return nil
}

// buildExternalService exposes only the MySQL client port: the status and API
// ports stay in-cluster. The Service is owned by the Instance (c.Apply), so it
// is garbage-collected with it.
func buildExternalService(c *controller.Context, e sqlEndpoint, spec *corev1alpha1.Service) *corev1.Service {
	meta := c.ObjectMeta(externalServiceName(c.Name(), e.component))
	meta.Annotations = spec.Annotations
	svc := &corev1.Service{
		ObjectMeta: meta,
		Spec: corev1.ServiceSpec{
			Type: spec.ServiceType,
			// The same pods the operator's own Service selects.
			Selector: map[string]string{
				tidbcorev1.LabelKeyManagedBy: tidbcorev1.LabelValManagedByOperator,
				tidbcorev1.LabelKeyComponent: e.label,
				tidbcorev1.LabelKeyCluster:   c.Name(),
				tidbcorev1.LabelKeyGroup:     c.Name(),
			},
			Ports: []corev1.ServicePort{{
				Name:       e.portName,
				Protocol:   corev1.ProtocolTCP,
				Port:       e.port,
				TargetPort: intstr.FromString(e.portName),
			}},
		},
	}
	if spec.ServiceType == corev1.ServiceTypeLoadBalancer && spec.LoadBalancerService != nil {
		if ranges := spec.LoadBalancerService.SourceRanges.NormalizedSourceRanges(); len(ranges) > 0 {
			svc.Spec.LoadBalancerSourceRanges = []string(ranges)
		}
	}
	return svc
}

// withExternalEndpoint points the connection details at the client
// component's external Service when it has one: the LoadBalancer address once
// assigned, or the node port of a NodePort Service, whose node addresses
// clients pick themselves. The in-cluster endpoint stays under internal_host
// and internal_port. Until a LoadBalancer address is assigned, host and port
// stay in-cluster.
func withExternalEndpoint(c *controller.Context, e sqlEndpoint, details controller.ConnectionDetails) controller.ConnectionDetails {
	if !exposed(c.Instance().Spec.Components[e.component].Service) {
		return details
	}
	svc := &corev1.Service{}
	if err := c.Get(svc, externalServiceName(c.Name(), e.component)); err != nil {
		return details
	}
	if details.AdditionalProperties == nil {
		details.AdditionalProperties = map[string]string{}
	}
	details.AdditionalProperties[connKeyInternalHost] = details.Host
	details.AdditionalProperties[connKeyInternalPort] = details.Port
	switch svc.Spec.Type {
	case corev1.ServiceTypeLoadBalancer:
		if address := loadBalancerAddress(svc); address != "" {
			details.Host = address
		}
	case corev1.ServiceTypeNodePort:
		for _, p := range svc.Spec.Ports {
			if p.Port == e.port && p.NodePort != 0 {
				details.AdditionalProperties[connKeyNodePort] = strconv.Itoa(int(p.NodePort))
			}
		}
	}
	return details
}

// loadBalancerAddress returns the hostname or, failing that, the IP the
// LoadBalancer was assigned.
func loadBalancerAddress(svc *corev1.Service) string {
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.Hostname != "" {
			return ingress.Hostname
		}
	}
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return ingress.IP
		}
	}
	return ""
}

// validateServices rejects service types the SQL components cannot be exposed
// with, and warns when TiDB is exposed while clients are pointed at TiProxy.
func validateServices(comps map[string]corev1alpha1.ComponentSpec) (field.ErrorList, []string) {
	var errs field.ErrorList
	for _, component := range exposedComponents {
		svc := comps[component].Service
		if svc == nil || svc.ServiceType == "" || slices.Contains(supportedServiceTypes, svc.ServiceType) {
			continue
		}
		errs = append(errs, field.NotSupported(componentsPath.Key(component).Child("service", "serviceType"),
			svc.ServiceType, supportedServiceTypes))
	}
	var warnings []string
	if _, ok := enabledTiProxy(comps); ok && exposed(comps[common.ComponentTiDB].Service) && !exposed(comps[common.ComponentTiProxy].Service) {
		warnings = append(warnings, "TiDB is exposed outside the cluster but TiProxy is not: connection details point at TiProxy "+
			"in-cluster, and external clients reach TiDB directly, bypassing TiProxy. Set spec.components.tiproxy.service to expose TiProxy instead.")
	}
	return errs, warnings
}
