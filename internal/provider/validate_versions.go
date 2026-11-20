package provider

import (
	"fmt"
	"strings"

	"golang.org/x/mod/semver"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// upgradeOrder is the order TiDB Operator rolls a version change out in; it
// holds each component back until the ones before it run at least its version.
var upgradeOrder = []string{common.ComponentPD, common.ComponentTiFlash, common.ComponentTiKV, common.ComponentTiDB}

// validateVersionSpec checks the requested component versions on their own:
// each is a semantic version and none is newer than one TiDB upgrades before it.
func validateVersionSpec(in *corev1alpha1.Instance, spec *corev1alpha1.ProviderSpec) error {
	target, err := targetVersions(in, spec)
	if err != nil {
		return err
	}
	if err := checkUpgradeOrder(target); err != nil {
		return err
	}
	to, err := tiproxyTargetVersion(in, spec)
	if err != nil {
		return err
	}
	if to != "" && !semver.IsValid(canonicalVersion(to)) {
		return fmt.Errorf("%s version %q is not a valid semantic version", common.ComponentTiProxy, to)
	}
	return nil
}

// validateNoDowngrade rejects requested versions older than the ones the
// cluster already targets, which TiDB cannot roll back to.
func validateNoDowngrade(c *controller.Context) error {
	spec, err := c.ProviderSpec()
	if err != nil {
		return err
	}
	target, err := targetVersions(c.Instance(), spec)
	if err != nil {
		return err
	}
	current, err := currentVersions(c)
	if err != nil {
		return err
	}
	if err := checkNoDowngrade(current, target); err != nil {
		return err
	}
	return validateTiProxyNoDowngrade(c, spec)
}

// tiproxyTargetVersion resolves TiProxy's requested version, or "" when it is
// disabled or unversioned. TiProxy has its own version line, so it is neither
// ordered against nor compared with the TiDB components.
func tiproxyTargetVersion(in *corev1alpha1.Instance, spec *corev1alpha1.ProviderSpec) (string, error) {
	comp, ok := enabledTiProxy(in.Spec.Components)
	if !ok {
		return "", nil
	}
	if comp.Version != "" {
		return comp.Version, nil
	}
	bundle, err := versionBundleComponents(in, spec)
	if err != nil {
		return "", err
	}
	return bundle[common.ComponentTiProxy], nil
}

func validateTiProxyNoDowngrade(c *controller.Context, spec *corev1alpha1.ProviderSpec) error {
	to, err := tiproxyTargetVersion(c.Instance(), spec)
	if err != nil || to == "" || !semver.IsValid(canonicalVersion(to)) {
		return err
	}
	group := &tidbcorev1.TiProxyGroup{}
	found, err := c.Exists(group, c.Name())
	if err != nil {
		return fmt.Errorf("reading current tiproxy version: %w", err)
	}
	from := group.Spec.Template.Spec.Version
	if !found || !semver.IsValid(canonicalVersion(from)) {
		return nil
	}
	if semver.Compare(canonicalVersion(to), canonicalVersion(from)) < 0 {
		return fmt.Errorf("downgrading %s from %s to %s is not supported", common.ComponentTiProxy, from, to)
	}
	return nil
}

// targetVersions resolves each running component's requested version: an
// explicit component version wins, otherwise the Instance's version bundle
// supplies it. A disabled TiFlash has no target.
func targetVersions(in *corev1alpha1.Instance, spec *corev1alpha1.ProviderSpec) (map[string]string, error) {
	comps := in.Spec.Components
	target := make(map[string]string, len(upgradeOrder))
	var bundleVersions map[string]string
	_, tiflashEnabled := enabledTiFlash(comps)
	for _, name := range upgradeOrder {
		if name == common.ComponentTiFlash && !tiflashEnabled {
			continue
		}
		version := comps[name].Version
		if version == "" {
			if bundleVersions == nil {
				var err error
				if bundleVersions, err = versionBundleComponents(in, spec); err != nil {
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

func versionBundleComponents(in *corev1alpha1.Instance, spec *corev1alpha1.ProviderSpec) (map[string]string, error) {
	if spec == nil {
		return map[string]string{}, nil
	}
	name := controller.EffectiveVersionBundleName(spec, in)
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

	tiflash := &tidbcorev1.TiFlashGroup{}
	if found, err := c.Exists(tiflash, c.Name()); err != nil {
		return nil, fmt.Errorf("reading current tiflash version: %w", err)
	} else if found {
		current[common.ComponentTiFlash] = tiflash.Spec.Template.Spec.Version
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
