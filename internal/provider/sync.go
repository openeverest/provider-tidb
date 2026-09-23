package provider

import (
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition/components"
	"github.com/openeverest/provider-tidb/internal/common"
)

// Fallbacks used when an Instance omits a value the UI normally supplies.
const (
	defaultPDReplicas   int32 = 3
	defaultTiKVReplicas int32 = 3
	defaultTiDBReplicas int32 = 2
)

var defaultVolumeSize = resource.MustParse("10Gi")

// SyncTiDB reconciles the Instance into a TiDB Operator v2 Cluster plus one
// group per component. Each resource carries an owner reference to the Instance
// (set by c.Apply), so deletion cascades automatically.
func SyncTiDB(c *controller.Context) error {
	providerSpec, err := c.ProviderSpec()
	if err != nil {
		return err
	}

	comps := c.Instance().Spec.Components

	// Generate (once) the root password and the bootstrap SQL that applies it.
	// The bootstrap ConfigMap must exist before the Cluster first bootstraps.
	password, err := ensureRootPassword(c)
	if err != nil {
		return err
	}
	if err := c.Apply(buildBootstrapSQLConfigMap(c, password)); err != nil {
		return err
	}

	// The Cluster is the shared root; every group references it by name.
	cluster, err := buildCluster(c)
	if err != nil {
		return err
	}
	if err := c.Apply(cluster); err != nil {
		return err
	}

	pd := comps[common.ComponentPD]
	if err := c.Apply(buildPDGroup(c, pd, resolveImage(providerSpec, common.ComponentPD, pd))); err != nil {
		return err
	}

	tikv := comps[common.ComponentTiKV]
	if err := c.Apply(buildTiKVGroup(c, tikv, resolveImage(providerSpec, common.ComponentTiKV, tikv))); err != nil {
		return err
	}

	tidb := comps[common.ComponentTiDB]
	if err := c.Apply(buildTiDBGroup(c, tidb, resolveImage(providerSpec, common.ComponentTiDB, tidb))); err != nil {
		return err
	}

	return nil
}

func buildCluster(c *controller.Context) (*tidbcorev1.Cluster, error) {
	cluster := &tidbcorev1.Cluster{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec:       tidbcorev1.ClusterSpec{},
	}
	// bootstrapSQL is immutable after creation: set it only for a new Cluster,
	// otherwise echo back the value the Cluster was created with.
	existing := &tidbcorev1.Cluster{}
	err := c.Get(existing, c.Name())
	switch {
	case apierrors.IsNotFound(err):
		cluster.Spec.BootstrapSQL = &corev1.LocalObjectReference{Name: bootstrapConfigMapName(c.Name())}
	case err != nil:
		return nil, err
	default:
		cluster.Spec.BootstrapSQL = existing.Spec.BootstrapSQL
	}
	return cluster, nil
}

func buildPDGroup(c *controller.Context, comp corev1alpha1.ComponentSpec, image string) *tidbcorev1.PDGroup {
	tmpl := tidbcorev1.PDTemplateSpec{
		Version:   comp.Version,
		Resources: toResources(comp.Resources),
		Volumes:   []tidbcorev1.Volume{dataVolume(comp.Storage, tidbcorev1.VolumeMountTypePDData)},
	}
	if image != "" {
		tmpl.Image = &image
	}
	if cfg := componentConfig(c, comp); cfg != "" {
		tmpl.Config = tidbcorev1.ConfigFile(cfg)
	}
	return &tidbcorev1.PDGroup{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec: tidbcorev1.PDGroupSpec{
			Cluster:  clusterRef(c),
			Replicas: replicasOrDefault(comp.Replicas, defaultPDReplicas),
			Template: tidbcorev1.PDTemplate{Spec: tmpl},
		},
	}
}

func buildTiKVGroup(c *controller.Context, comp corev1alpha1.ComponentSpec, image string) *tidbcorev1.TiKVGroup {
	tmpl := tidbcorev1.TiKVTemplateSpec{
		Version:   comp.Version,
		Resources: toResources(comp.Resources),
		Volumes:   []tidbcorev1.Volume{dataVolume(comp.Storage, tidbcorev1.VolumeMountTypeTiKVData)},
	}
	if image != "" {
		tmpl.Image = &image
	}
	if cfg := componentConfig(c, comp); cfg != "" {
		tmpl.Config = tidbcorev1.ConfigFile(cfg)
	}
	return &tidbcorev1.TiKVGroup{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec: tidbcorev1.TiKVGroupSpec{
			Cluster:  clusterRef(c),
			Replicas: replicasOrDefault(comp.Replicas, defaultTiKVReplicas),
			Template: tidbcorev1.TiKVTemplate{Spec: tmpl},
		},
	}
}

func buildTiDBGroup(c *controller.Context, comp corev1alpha1.ComponentSpec, image string) *tidbcorev1.TiDBGroup {
	// TiDB is stateless: no data volume.
	tmpl := tidbcorev1.TiDBTemplateSpec{
		Version:   comp.Version,
		Resources: toResources(comp.Resources),
	}
	if image != "" {
		tmpl.Image = &image
	}
	if cfg := componentConfig(c, comp); cfg != "" {
		tmpl.Config = tidbcorev1.ConfigFile(cfg)
	}
	return &tidbcorev1.TiDBGroup{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec: tidbcorev1.TiDBGroupSpec{
			Cluster:  clusterRef(c),
			Replicas: replicasOrDefault(comp.Replicas, defaultTiDBReplicas),
			Template: tidbcorev1.TiDBTemplate{Spec: tmpl},
		},
	}
}

func clusterRef(c *controller.Context) tidbcorev1.ClusterReference {
	return tidbcorev1.ClusterReference{Name: c.Name()}
}

// resolveImage picks the container image for a component: an explicit user
// override wins, otherwise the provider's version catalog resolves it from the
// component's version, falling back to the component's default image.
func resolveImage(spec *corev1alpha1.ProviderSpec, name string, comp corev1alpha1.ComponentSpec) string {
	if comp.Image != "" {
		return comp.Image
	}
	if comp.Version != "" {
		if img := controller.GetImageForVersion(spec, name, comp.Version); img != "" {
			return img
		}
	}
	return controller.GetDefaultImageForComponent(spec, name)
}

// componentConfig decodes the inline TOML config from a component's parameters.
func componentConfig(c *controller.Context, comp corev1alpha1.ComponentSpec) string {
	var params components.PdParameters // all component params share the `config` field
	if !c.TryDecodeComponentParameters(comp, &params) {
		return ""
	}
	return params.Config
}

// toResources maps the Instance resource limits onto the operator's simplified
// (requests==limits) resource model.
func toResources(r *corev1.ResourceRequirements) tidbcorev1.ResourceRequirements {
	out := tidbcorev1.ResourceRequirements{}
	if r == nil || r.Limits == nil {
		return out
	}
	if cpu := r.Limits.Cpu(); cpu != nil && !cpu.IsZero() {
		q := cpu.DeepCopy()
		out.CPU = &q
	}
	if mem := r.Limits.Memory(); mem != nil && !mem.IsZero() {
		q := mem.DeepCopy()
		out.Memory = &q
	}
	return out
}

// dataVolume builds the required `data` volume for a stateful component.
func dataVolume(s *corev1alpha1.Storage, mountType tidbcorev1.VolumeMountType) tidbcorev1.Volume {
	v := tidbcorev1.Volume{
		Name:    "data",
		Mounts:  []tidbcorev1.VolumeMount{{Type: mountType}},
		Storage: defaultVolumeSize,
	}
	if s != nil {
		if !s.Size.IsZero() {
			v.Storage = s.Size
		}
		if s.StorageClass != nil {
			v.StorageClassName = s.StorageClass
		}
	}
	return v
}

func replicasOrDefault(r *int32, def int32) *int32 {
	if r != nil {
		return r
	}
	return &def
}
