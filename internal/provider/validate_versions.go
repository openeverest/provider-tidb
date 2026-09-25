package provider

import (
	"fmt"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// upgradeOrder is the order TiDB Operator rolls a version change out in; it
// holds each component back until the ones before it run at least its version.
var upgradeOrder = []string{common.ComponentPD, common.ComponentTiKV, common.ComponentTiDB}

// validateVersions enforces TiDB's upgrade rules on the requested component
// versions, comparing them with what the cluster already targets.
func validateVersions(c *controller.Context) error {
	target, err := targetVersions(c)
	if err != nil {
		return err
	}
	if err := checkUpgradeOrder(target); err != nil {
		return err
	}
	current, err := currentVersions(c)
	if err != nil {
		return err
	}
	return checkNoDowngrade(current, target)
}

// targetVersions resolves each component's requested version: an explicit
// component version wins, otherwise the Instance's version bundle supplies it.
func targetVersions(c *controller.Context) (map[string]string, error) {
	comps := c.Instance().Spec.Components
	target := make(map[string]string, len(upgradeOrder))
	var bundleVersions map[string]string
	for _, name := range upgradeOrder {
		version := comps[name].Version
		if version == "" {
			if bundleVersions == nil {
				var err error
				if bundleVersions, err = versionBundleComponents(c); err != nil {
					return nil, err
				}
			}
			version = bundleVersions[name]
		}
		if version != "" {
			target[name] = version
		}
	}
	return target, nil
}

func versionBundleComponents(c *controller.Context) (map[string]string, error) {
	spec, err := c.ProviderSpec()
	if err != nil {
		return nil, err
	}
	name := controller.EffectiveVersionBundleName(spec, c.Instance())
	if name == "" {
		return map[string]string{}, nil
	}
	bundle, err := controller.ResolveVersionBundle(spec, name)
	if err != nil {
		return nil, err
	}
	return bundle.Components, nil
}

// currentVersions reads the version each existing group was last told to run.
// Groups that do not exist yet (a new Instance) are absent from the result.
func currentVersions(c *controller.Context) (map[string]string, error) {
	current := make(map[string]string, len(upgradeOrder))

	pd := &tidbcorev1.PDGroup{}
	if found, err := c.Exists(pd, c.Name()); err != nil {
		return nil, fmt.Errorf("reading current pd version: %w", err)
	} else if found {
		current[common.ComponentPD] = pd.Spec.Template.Spec.Version
	}

	tikv := &tidbcorev1.TiKVGroup{}
	if found, err := c.Exists(tikv, c.Name()); err != nil {
		return nil, fmt.Errorf("reading current tikv version: %w", err)
	} else if found {
		current[common.ComponentTiKV] = tikv.Spec.Template.Spec.Version
	}

	tidb := &tidbcorev1.TiDBGroup{}
	if found, err := c.Exists(tidb, c.Name()); err != nil {
		return nil, fmt.Errorf("reading current tidb version: %w", err)
	} else if found {
		current[common.ComponentTiDB] = tidb.Spec.Template.Spec.Version
	}

	return current, nil
}

// checkUpgradeOrder rejects version sets the operator would never finish
// rolling out: a component newer than one that must be upgraded before it.
func checkUpgradeOrder(target map[string]string) error {
	for i, name := range upgradeOrder {
		version, ok := target[name]
		if !ok {
			continue
		}
		if !semver.IsValid(canonicalVersion(version)) {
			return fmt.Errorf("%s version %q is not a valid semantic version", name, version)
		}
		for _, before := range upgradeOrder[:i] {
			beforeVersion, ok := target[before]
			if ok && semver.Compare(canonicalVersion(version), canonicalVersion(beforeVersion)) > 0 {
				return fmt.Errorf("%s version %s is newer than %s version %s: TiDB upgrades %s in that order, "+
					"so a component cannot target a newer version than the ones upgraded before it",
					name, version, before, beforeVersion, strings.Join(upgradeOrder, " → "))
			}
		}
	}
	return nil
}

// checkNoDowngrade rejects moving any component to an earlier version, which
// TiDB does not support.
func checkNoDowngrade(current, target map[string]string) error {
	for _, name := range upgradeOrder {
		from, ok := current[name]
		to, wanted := target[name]
		if !ok || !wanted || !semver.IsValid(canonicalVersion(from)) {
			continue
		}
		if semver.Compare(canonicalVersion(to), canonicalVersion(from)) < 0 {
			return fmt.Errorf("downgrading %s from %s to %s is not supported: TiDB cannot be rolled back to an earlier version",
				name, from, to)
		}
	}
	return nil
}

// canonicalVersion adds the "v" prefix golang.org/x/mod/semver requires.
func canonicalVersion(v string) string {
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}
