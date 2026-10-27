package provider

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"
)

const pdDNSWaitContainer = "wait-for-dns"

// pdDNSWaitScript waits until the pod's own peer DNS name resolves, for at most
// three minutes. It asks DNS directly: /etc/hosts already has the pod's name.
const pdDNSWaitScript = `for i in $(seq 180); do
  nslookup "$(hostname).${PEER_DOMAIN}" >/dev/null 2>&1 && exit 0
  sleep 1
done
echo "DNS name $(hostname).${PEER_DOMAIN} still does not resolve; starting PD anyway"`

// addPDDNSWait holds PD back until its peer DNS name resolves. PD's embedded
// etcd looks up its own peer URL once at startup and exits when that fails.
// While a new cluster comes up the names don't exist yet, CoreDNS caches the
// miss (for 30s on most clusters), and PD crash-loops two or three times with
// growing back-off before it starts.
//
// Only new PD groups get the init container, so upgrading the provider does
// not roll the PD pods of existing clusters.
func addPDDNSWait(c *controller.Context, group *tidbcorev1.PDGroup, image string) error {
	if image == "" {
		return nil
	}
	existing := &tidbcorev1.PDGroup{}
	switch err := c.Get(existing, group.Name); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("get PD group: %w", err)
	case !hasPDDNSWait(existing):
		return nil
	}

	overlay := group.Spec.Template.Spec.Overlay
	if overlay == nil {
		overlay = &tidbcorev1.Overlay{}
		group.Spec.Template.Spec.Overlay = overlay
	}
	if overlay.Pod == nil {
		overlay.Pod = &tidbcorev1.PodOverlay{}
	}
	if overlay.Pod.Spec == nil {
		overlay.Pod.Spec = &corev1.PodSpec{}
	}
	// Requests equal limits so the pod keeps the Guaranteed QoS class PD has.
	small := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("64Mi"),
	}
	overlay.Pod.Spec.InitContainers = append(overlay.Pod.Spec.InitContainers, corev1.Container{
		Name:      pdDNSWaitContainer,
		Image:     image,
		Command:   []string{"/bin/sh", "-c", pdDNSWaitScript},
		Env:       []corev1.EnvVar{{Name: "PEER_DOMAIN", Value: fmt.Sprintf("%s-pd-peer.%s", group.Name, c.Namespace())}},
		Resources: corev1.ResourceRequirements{Requests: small, Limits: small},
	})
	return nil
}

func hasPDDNSWait(group *tidbcorev1.PDGroup) bool {
	o := group.Spec.Template.Spec.Overlay
	if o == nil || o.Pod == nil || o.Pod.Spec == nil {
		return false
	}
	for _, ic := range o.Pod.Spec.InitContainers {
		if ic.Name == pdDNSWaitContainer {
			return true
		}
	}
	return false
}
