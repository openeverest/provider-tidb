package provider

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"
	metav1alpha1 "github.com/pingcap/tidb-operator/api/v2/meta/v1alpha1"
)

// TiProxy moves a session to another TiDB with a token the old TiDB signs and
// the new one verifies, so every TiDB needs the same signing certificate. TiDB
// Operator mounts it and sets session-token-signing-cert/-key only under the
// SessionTokenSigning feature gate, from the Secret named in
// spec.security.sessionTokenSigningCertKeyPair (by default the TiDB cluster
// certificate, which a cluster without TLS does not have). The provider issues
// its own signing certificate, with or without TLS.
//
// The Cluster CRD freezes spec.featureGates unless FeatureModification is
// already on, and an instance that lacks FeatureModification takes the gates
// straight from the Cluster: TiDB Operator would then add the signing volume to
// running TiDB pods in place, which Kubernetes rejects. So on an existing
// cluster the provider first turns on FeatureModification (no restart), waits
// until every TiDB instance carries it, and only then SessionTokenSigning, which
// the operator rolls out by replacing the TiDB pods one at a time. A new
// cluster gets both gates up front.
//
// The gates and the Secret stay once set: turning them off would roll TiDB
// again, and they cost nothing while TiProxy is off.

const sessionTokenSigningSecretSuffix = "-session-token-signing-secret"

func sessionTokenSigningSecretName(instance string) string {
	return instance + sessionTokenSigningSecretSuffix
}

func sessionTokenSigningLeaf(instance string) leafCert {
	return leafCert{
		secretName: sessionTokenSigningSecretName(instance),
		commonName: instance + "-tidb-session-token",
	}
}

func featureGate(name metav1alpha1.Feature) metav1alpha1.FeatureGate {
	return metav1alpha1.FeatureGate{Name: name}
}

func hasFeatureGate(cluster *tidbcorev1.Cluster, name metav1alpha1.Feature) bool {
	return slices.ContainsFunc(cluster.Spec.FeatureGates, func(g metav1alpha1.FeatureGate) bool { return g.Name == name })
}

func sessionTokenSigningSecurity(instance string) *tidbcorev1.ClusterSecurity {
	return &tidbcorev1.ClusterSecurity{
		SessionTokenSigningCertKeyPair: &corev1.LocalObjectReference{Name: sessionTokenSigningSecretName(instance)},
	}
}

// newClusterSessionTokenSigning returns the feature gates and security settings
// of a new Cluster: both gates and the signing Secret when it starts with TiProxy.
func newClusterSessionTokenSigning(c *controller.Context) ([]metav1alpha1.FeatureGate, *tidbcorev1.ClusterSecurity) {
	if _, ok := enabledTiProxy(c.Instance().Spec.Components); !ok {
		return nil, nil
	}
	return []metav1alpha1.FeatureGate{
		featureGate(metav1alpha1.FeatureModification),
		featureGate(metav1alpha1.SessionTokenSigning),
	}, sessionTokenSigningSecurity(c.Name())
}

// existingClusterSessionTokenSigning returns the feature gates and security
// settings of an existing Cluster: what it already has, plus the next step
// towards session token signing while TiProxy is enabled.
func existingClusterSessionTokenSigning(c *controller.Context, existing *tidbcorev1.Cluster) (
	[]metav1alpha1.FeatureGate, *tidbcorev1.ClusterSecurity, error) {
	_, tiproxy := enabledTiProxy(c.Instance().Spec.Components)
	modification := hasFeatureGate(existing, metav1alpha1.FeatureModification)
	signing := hasFeatureGate(existing, metav1alpha1.SessionTokenSigning)

	var gates []metav1alpha1.FeatureGate
	if modification || tiproxy {
		gates = append(gates, featureGate(metav1alpha1.FeatureModification))
	}
	if signing {
		gates = append(gates, featureGate(metav1alpha1.SessionTokenSigning))
		var security *tidbcorev1.ClusterSecurity
		if sec := existing.Spec.Security; sec != nil && sec.SessionTokenSigningCertKeyPair != nil {
			security = &tidbcorev1.ClusterSecurity{SessionTokenSigningCertKeyPair: sec.SessionTokenSigningCertKeyPair.DeepCopy()}
		}
		return gates, security, nil
	}
	if !tiproxy || !modification {
		return gates, nil, nil
	}
	ready, err := readyForSessionTokenSigning(c)
	if err != nil || !ready {
		return gates, nil, err
	}
	return append(gates, featureGate(metav1alpha1.SessionTokenSigning)), sessionTokenSigningSecurity(c.Name()), nil
}

// readyForSessionTokenSigning reports whether SessionTokenSigning can be turned
// on: the signing Secret exists and every TiDB instance already carries
// FeatureModification, so the operator replaces the TiDB pods instead of
// updating them in place.
func readyForSessionTokenSigning(c *controller.Context) (bool, error) {
	found, err := c.Exists(&corev1.Secret{}, sessionTokenSigningSecretName(c.Name()))
	if err != nil || !found {
		return false, err
	}
	group := &tidbcorev1.TiDBGroup{}
	found, err = c.Exists(group, c.Name())
	if err != nil {
		return false, fmt.Errorf("get tidb group: %w", err)
	}
	if !found {
		return true, nil
	}
	if !slices.Contains(group.Spec.Features, metav1alpha1.FeatureModification) {
		return false, nil
	}
	tidbs := &tidbcorev1.TiDBList{}
	if err := c.List(tidbs, client.MatchingLabels{
		tidbcorev1.LabelKeyCluster: c.Name(),
		tidbcorev1.LabelKeyGroup:   c.Name(),
	}); err != nil {
		return false, fmt.Errorf("listing tidb instances: %w", err)
	}
	for i := range tidbs.Items {
		if !slices.Contains(tidbs.Items[i].Spec.Features, metav1alpha1.FeatureModification) {
			return false, nil
		}
	}
	return true, nil
}

// usesSessionTokenSigning reports whether the Instance needs the signing
// certificate: TiProxy is enabled, or the Cluster already points TiDB at it.
func usesSessionTokenSigning(c *controller.Context, cluster *tidbcorev1.Cluster) bool {
	if _, ok := enabledTiProxy(c.Instance().Spec.Components); ok {
		return true
	}
	sec := cluster.Spec.Security
	return sec != nil && sec.SessionTokenSigningCertKeyPair != nil &&
		sec.SessionTokenSigningCertKeyPair.Name == sessionTokenSigningSecretName(c.Name())
}
