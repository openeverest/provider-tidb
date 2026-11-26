package provider

import (
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-tidb/internal/common"
)

// maxClusterNameLength mirrors the TiDB Operator Cluster CRD constraint
// (metadata.name must not exceed 37 characters).
const maxClusterNameLength = 37

var _ controller.ReferenceValidator = (*Provider)(nil)

var (
	componentsPath = field.NewPath("spec", "components")
	tikvPath       = componentsPath.Key(common.ComponentTiKV)
)

// Validate checks the rules about the Instance alone. The runtime runs it at
// admission, where its errors deny the write, and before every Sync.
func (p *Provider) Validate(in controller.ValidationInput) controller.ValidationResult {
	instance := in.Instance
	var errs field.ErrorList
	if len(instance.Name) > maxClusterNameLength {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), instance.Name,
			fmt.Sprintf("instance name %q is too long: TiDB cluster name must not exceed %d characters",
				instance.Name, maxClusterNameLength)))
	}

	comps := instance.Spec.Components
	for _, name := range []string{common.ComponentPD, common.ComponentTiKV, common.ComponentTiDB} {
		if _, ok := comps[name]; !ok {
			errs = append(errs, field.Required(componentsPath.Key(name), fmt.Sprintf("required component %q is missing", name)))
		}
	}
	if len(errs) > 0 {
		return controller.ValidationResult{Errors: errs}
	}

	// PD forms a quorum (etcd/raft), so an even count is never valid.
	if pd := comps[common.ComponentPD]; pd.Replicas != nil && *pd.Replicas%2 == 0 {
		errs = append(errs, field.Invalid(componentsPath.Key(common.ComponentPD).Child("replicas"), *pd.Replicas,
			fmt.Sprintf("pd replicas must be an odd number, got %d", *pd.Replicas)))
	}

	var spec *corev1alpha1.ProviderSpec
	if in.Provider != nil {
		spec = &in.Provider.Spec
	}
	errs = append(errs, invalid(componentsPath, validateVersionSpec(instance, spec))...)
	errs = append(errs, validateTLSParameters(instance, in.Old)...)
	errs = append(errs, validateUserSecretRef(instance, in.Old)...)
	return controller.ValidationResult{Errors: errs}
}

// ValidateReferences checks the rules that read the cluster: the operator a
// setting needs, and changes the live TiDB groups cannot take, such as a
// downgrade, a TiKV scale-in below PD's replica count, a smaller volume or new
// TLS settings. At admission its errors are warnings; before Sync they block it.
func (p *Provider) ValidateReferences(c *controller.Context) (field.ErrorList, error) {
	var errs field.ErrorList
	for _, check := range []struct {
		path *field.Path
		run  func(*controller.Context) error
	}{
		{componentsPath, validateNoDowngrade},
		{tikvPath.Child("replicas"), validateTiKVScaleIn},
		{tikvPath.Child("resources", "limits", "memory"), validateTiKVMemory},
		{tlsPath, validateTLS},
		{componentsPath, validateStorage},
		{userSecretRefPath.Child("name"), validateRootSecret},
	} {
		err := check.run(c)
		if err == nil {
			continue
		}
		// A read that failed for another reason than a missing object is retried.
		if status := apierrors.APIStatus(nil); errors.As(err, &status) && !apierrors.IsNotFound(err) {
			return nil, err
		}
		errs = append(errs, field.Invalid(check.path, field.OmitValueType{}, err.Error()))
	}
	return errs, nil
}

// invalid reports err, if any, as an invalid value of the field at path.
func invalid(path *field.Path, err error) field.ErrorList {
	if err == nil {
		return nil
	}
	return field.ErrorList{field.Invalid(path, field.OmitValueType{}, err.Error())}
}
