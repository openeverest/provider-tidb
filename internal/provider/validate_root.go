package provider

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/pkg/common"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// rootSecretDefinition is the provider's secret type for spec.userSecretRef.
const rootSecretDefinition = "root"

var userSecretRefPath = field.NewPath("spec", "userSecretRef")

// validateUserSecretRef checks where spec.userSecretRef may be set. TiDB runs
// the bootstrap SQL that sets the root password once, when the cluster is
// created, so the Secret can only be named then; and a restore brings back the
// backed-up cluster's root password, which the Secret would contradict.
func validateUserSecretRef(instance, old *corev1alpha1.Instance) field.ErrorList {
	if instance.Spec.UserSecretRef == nil {
		return nil
	}
	if instance.Spec.DataSource != nil {
		return field.ErrorList{field.Forbidden(userSecretRefPath,
			"must not be set with spec.dataSource: the restored data brings the root password of the cluster it was backed up from")}
	}
	if old != nil && old.Spec.UserSecretRef == nil {
		return field.ErrorList{field.Forbidden(userSecretRefPath,
			"can only be set when the Instance is created: the root password is applied once, when the cluster bootstraps")}
	}
	return nil
}

// validateRootSecret checks the Secret spec.userSecretRef names until the
// cluster has taken its password; after that the Secret is no longer read.
func validateRootSecret(c *controller.Context) error {
	ref := c.Instance().Spec.UserSecretRef
	if ref == nil {
		return nil
	}
	if readRootPassword(c) != "" {
		return nil
	}
	secret := &corev1.Secret{}
	if err := c.Get(secret, ref.Name); err != nil {
		return err
	}
	if err := checkRootPassword(string(secret.Data[rootPasswordKey])); err != nil {
		return fmt.Errorf("secret %q: %w", ref.Name, err)
	}
	// A Secret created for the root definition, as the UI does, also fits its schema.
	if secret.Labels[common.OpenEverestDefinitionLabel] != rootSecretDefinition {
		return nil
	}
	spec, err := c.ProviderSpec()
	if err != nil {
		return fmt.Errorf("get provider spec: %w", err)
	}
	if err := controller.ValidateSecretSchema(secret, spec); err != nil {
		return fmt.Errorf("secret %q: %w", ref.Name, err)
	}
	return nil
}
