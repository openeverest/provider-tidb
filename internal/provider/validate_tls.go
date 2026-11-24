package provider

import (
	"encoding/json"
	"net"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"

	"github.com/openeverest/provider-tidb/definition"
)

var (
	tlsPath       = field.NewPath("spec", "parameters", "tls")
	legacyTLSPath = field.NewPath("spec", "topology", "parameters", "tls")
)

// validateTLSParameters checks spec.parameters.tls alone and, on an update,
// against the old Instance.
func validateTLSParameters(instance, old *corev1alpha1.Instance) field.ErrorList {
	if hasLegacyTLS(instance) {
		return field.ErrorList{field.Invalid(legacyTLSPath, field.OmitValueType{},
			"TLS moved to spec.parameters.tls: tls {cluster: true, client: true} is now mode: preferTLS and "+
				"certManager.issuerRef is now issuer; see the provider README for the migration")}
	}
	params, err := decodeTLSParameters(instance)
	if err != nil {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "parameters"), field.OmitValueType{}, err.Error())}
	}
	var errs field.ErrorList
	modes := []string{string(definition.TLSModeDisabled), string(definition.TLSModePrefer), string(definition.TLSModeRequire)}
	if params.Mode != "" && !slices.Contains(modes, string(params.Mode)) {
		errs = append(errs, field.NotSupported(tlsPath.Child("mode"), params.Mode, modes))
	}
	if !tlsEnabled(params) {
		if params.Issuer != nil {
			errs = append(errs, field.Forbidden(tlsPath.Child("issuer"), "needs mode preferTLS or requireTLS"))
		}
		if len(params.AdditionalNames) > 0 {
			errs = append(errs, field.Forbidden(tlsPath.Child("additionalNames"), "needs mode preferTLS or requireTLS"))
		}
	}
	errs = append(errs, validateIssuer(params.Issuer)...)
	errs = append(errs, validateAdditionalNames(params.AdditionalNames)...)
	if len(errs) > 0 || old == nil || hasLegacyTLS(old) {
		// An Instance still in the old shape is migrating: the Cluster it
		// created is what the new settings must match (validateTLS).
		return errs
	}
	oldParams, err := decodeTLSParameters(old)
	if err != nil {
		return errs
	}
	if tlsEnabled(oldParams) != tlsEnabled(params) {
		errs = append(errs, field.Forbidden(tlsPath.Child("mode"),
			"TiDB Operator can't switch a running cluster between plaintext and TLS: disabled can only be set when the Instance is created"))
	}
	if tlsEnabled(oldParams) && issuerSourceOf(oldParams.Issuer) != issuerSourceOf(params.Issuer) {
		errs = append(errs, field.Forbidden(tlsPath.Child("issuer"),
			"can only be set when the Instance is created: a new CA would not verify the certificates the pods use"))
	}
	return errs
}

// hasLegacyTLS reports whether the Instance still has TLS in the topology
// parameters, where provider versions up to 0.3 read it.
func hasLegacyTLS(instance *corev1alpha1.Instance) bool {
	raw := instance.GetTopologyParameters()
	if raw == nil || len(raw.Raw) == 0 {
		return false
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(raw.Raw, &params) != nil {
		return false
	}
	_, ok := params["tls"]
	return ok
}

func issuerSourceOf(ref *definition.IssuerReference) string {
	if ref == nil {
		return ""
	}
	return issuerSource(*ref)
}

func validateIssuer(ref *definition.IssuerReference) field.ErrorList {
	if ref == nil {
		return nil
	}
	path := tlsPath.Child("issuer")
	var errs field.ErrorList
	if ref.Name == "" {
		errs = append(errs, field.Required(path.Child("name"), "the name of the cert-manager issuer"))
	}
	if issuerGroup(*ref) == cmapi.SchemeGroupVersion.Group {
		kinds := []string{string(definition.IssuerKindIssuer), string(definition.IssuerKindClusterIssuer)}
		if !slices.Contains(kinds, issuerKind(*ref)) {
			errs = append(errs, field.NotSupported(path.Child("kind"), ref.Kind, kinds))
		}
	}
	return errs
}

// validateAdditionalNames accepts DNS names, with an optional leading
// wildcard label. IP addresses are not offered.
func validateAdditionalNames(names []string) field.ErrorList {
	var errs field.ErrorList
	for i, name := range names {
		path := tlsPath.Child("additionalNames").Index(i)
		if net.ParseIP(name) != nil {
			errs = append(errs, field.Invalid(path, name, "must be a DNS name, not an IP address"))
			continue
		}
		msgs := validation.IsDNS1123Subdomain(name)
		if strings.HasPrefix(name, "*.") {
			msgs = validation.IsWildcardDNS1123Subdomain(name)
		}
		if strings.Contains(name, ",") {
			msgs = append(msgs, "must not contain a comma")
		}
		for _, msg := range msgs {
			errs = append(errs, field.Invalid(path, name, msg))
		}
	}
	return errs
}
