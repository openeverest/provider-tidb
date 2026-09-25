package provider

import (
	"fmt"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// validateStorage rejects shrinking the data volume of a stateful component:
// Kubernetes can only expand a PersistentVolumeClaim in place.
func validateStorage(c *controller.Context) error {
	comps := c.Instance().Spec.Components

	pd := &tidbcorev1.PDGroup{}
	if found, err := c.Exists(pd, c.Name()); err != nil {
		return fmt.Errorf("reading current pd storage: %w", err)
	} else if found {
		if err := checkNoShrink(common.ComponentPD, pd.Spec.Template.Spec.Volumes, comps[common.ComponentPD].Storage); err != nil {
			return err
		}
	}

	tikv := &tidbcorev1.TiKVGroup{}
	if found, err := c.Exists(tikv, c.Name()); err != nil {
		return fmt.Errorf("reading current tikv storage: %w", err)
	} else if found {
		if err := checkNoShrink(common.ComponentTiKV, tikv.Spec.Template.Spec.Volumes, comps[common.ComponentTiKV].Storage); err != nil {
			return err
		}
	}

	return nil
}

func checkNoShrink(component string, current []tidbcorev1.Volume, requested *corev1alpha1.Storage) error {
	desired := dataVolumeSize(requested)
	for _, v := range current {
		if v.Name == dataVolumeName && desired.Cmp(v.Storage) < 0 {
			return fmt.Errorf("shrinking %s storage from %s to %s is not supported: volumes can only be expanded",
				component, v.Storage.String(), desired.String())
		}
	}
	return nil
}
