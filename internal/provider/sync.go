package provider

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition/components"
	"github.com/openeverest/provider-tidb/internal/common"
)

// Fallbacks used when an Instance omits a value the UI normally supplies.
const (
	defaultPDReplicas      int32 = 3
	defaultTiKVReplicas    int32 = 3
	defaultTiDBReplicas    int32 = 2
	defaultTiFlashReplicas int32 = 1
)

var defaultVolumeSize = resource.MustParse("10Gi")

const dataVolumeName = "data"

// SyncTiDB reconciles the Instance into a TiDB Operator v2 Cluster plus one
// group per component. The Cluster is owned by the Instance; the component
// groups are owned by the Cluster so that deleting the Instance cascades
// through the operator's ordered Cluster teardown (see applyWithOwner).
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

	// The Cluster is the shared root and is owned by the Instance, so deleting
	// the Instance garbage-collects it and the operator's Cluster finalizer then
	// tears the groups down in the correct order. It must be applied with
	// applyWithOwner (not c.Apply) so the operator's finalizer is preserved.
	cluster, err := buildCluster(c)
	if err != nil {
		return err
	}
	if err := applyWithOwner(c, c.Instance(), cluster); err != nil {
		return err
	}
	// Re-read the Cluster so its UID is available for the group owner references.
	owner := &tidbcorev1.Cluster{}
	if err := c.Get(owner, c.Name()); err != nil {
		return fmt.Errorf("get cluster for ownership: %w", err)
	}

	pd := comps[common.ComponentPD]
	if err := applyWithOwner(c, owner, buildPDGroup(c, pd, resolveImage(providerSpec, common.ComponentPD, pd))); err != nil {
		return err
	}

	tikv := comps[common.ComponentTiKV]
	if err := applyWithOwner(c, owner, buildTiKVGroup(c, tikv, resolveImage(providerSpec, common.ComponentTiKV, tikv))); err != nil {
		return err
	}

	tidb := comps[common.ComponentTiDB]
	if err := applyWithOwner(c, owner, buildTiDBGroup(c, tidb, resolveImage(providerSpec, common.ComponentTiDB, tidb))); err != nil {
		return err
	}

	if err := syncTiFlash(c, providerSpec, owner); err != nil {
		return err
	}

	return reconcileDataSource(c)
}

// syncTiFlash applies the TiFlash group while TiFlash is enabled and deletes it
// once disabled; the operator offlines each TiFlash store before removing it.
func syncTiFlash(c *controller.Context, providerSpec *corev1alpha1.ProviderSpec, owner *tidbcorev1.Cluster) error {
	if tiflash, ok := enabledTiFlash(c.Instance().Spec.Components); ok {
		return applyWithOwner(c, owner, buildTiFlashGroup(c, tiflash, resolveImage(providerSpec, common.ComponentTiFlash, tiflash)))
	}
	group := &tidbcorev1.TiFlashGroup{}
	found, err := c.Exists(group, c.Name())
	if err != nil {
		return fmt.Errorf("get tiflash group: %w", err)
	}
	if !found || !group.DeletionTimestamp.IsZero() {
		return nil
	}
	return c.Delete(group)
}

// enabledTiFlash returns the TiFlash component when the Instance runs it: the
// component is present and does not ask for zero replicas.
func enabledTiFlash(comps map[string]corev1alpha1.ComponentSpec) (corev1alpha1.ComponentSpec, bool) {
	comp, ok := comps[common.ComponentTiFlash]
	if !ok || (comp.Replicas != nil && *comp.Replicas == 0) {
		return corev1alpha1.ComponentSpec{}, false
	}
	return comp, true
}

// applyWithOwner creates or updates obj with the given controller owner,
// preserving any finalizers the operator has added.
//
// Ordered teardown depends on two things this helper guarantees:
//   - The component groups are owned by the Cluster (not the Instance), so
//     deleting the Instance garbage-collects only the Cluster. While the
//     Cluster is held by its finalizer, its Cluster-owned groups are not
//     concurrently garbage-collected; the operator deletes them in sequence.
//     Instance ownership would GC every group at once, killing PD mid-eviction
//     and deadlocking TiKV on its "leaders are not all evicted" finalizer.
//   - The operator's finalizer (e.g. pingcap.com/finalizer) is preserved on
//     update. A blind replace would strip it, so deleting the Cluster would
//     hard-delete it instead of triggering the operator's ordered teardown,
//     orphaning the groups (their controllers fail with "cannot get cluster").
func applyWithOwner(c *controller.Context, owner, obj client.Object) error {
	if err := controllerutil.SetControllerReference(owner, obj, c.Client().Scheme()); err != nil {
		return fmt.Errorf("set owner: %w", err)
	}
	existing := obj.DeepCopyObject().(client.Object)
	err := c.Client().Get(c.Context(), client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		return c.Client().Create(c.Context(), obj)
	}
	if err != nil {
		return err
	}
	obj.SetFinalizers(existing.GetFinalizers())
	obj.SetResourceVersion(existing.GetResourceVersion())
	return c.Client().Update(c.Context(), obj)
}

// reconcileDataSource seeds a new Instance from .spec.dataSource once the
// cluster can accept a restore. The runtime creates and tracks the Restore CR
// (SyncRestore turns it into a BR Restore); the Instance is held in Restoring
// until it completes.
func reconcileDataSource(c *controller.Context) error {
	if c.Instance().Spec.DataSource == nil {
		return nil
	}
	if !clusterReady(c) {
		c.SetDataSourceStatus(controller.DataSourceStatus{
			Done:    false,
			State:   controller.DataSourceStateWaiting,
			Reason:  corev1alpha1.ReasonDataSourceWaitingForCluster,
			Message: "Waiting for the TiDB cluster to be ready before restoring",
		})
		return nil
	}
	if _, err := c.ReconcileDataSource(); err != nil {
		return fmt.Errorf("reconcile data source: %w", err)
	}
	return nil
}

// clusterReady reports whether every component group has its desired replicas ready.
func clusterReady(c *controller.Context) bool {
	pd := &tidbcorev1.PDGroup{}
	tikv := &tidbcorev1.TiKVGroup{}
	tidb := &tidbcorev1.TiDBGroup{}
	if c.Get(pd, c.Name()) != nil || c.Get(tikv, c.Name()) != nil || c.Get(tidb, c.Name()) != nil {
		return false
	}
	return groupIsReady(pd.Spec.Replicas, pd.Status.ReadyReplicas) &&
		groupIsReady(tikv.Spec.Replicas, tikv.Status.ReadyReplicas) &&
		groupIsReady(tidb.Spec.Replicas, tidb.Status.ReadyReplicas)
}

func groupIsReady(desired *int32, ready int32) bool {
	return desired != nil && *desired > 0 && ready >= *desired
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

func buildTiFlashGroup(c *controller.Context, comp corev1alpha1.ComponentSpec, image string) *tidbcorev1.TiFlashGroup {
	tmpl := tidbcorev1.TiFlashTemplateSpec{
		Version:   comp.Version,
		Resources: toResources(comp.Resources),
		Volumes:   []tidbcorev1.Volume{dataVolume(comp.Storage, tidbcorev1.VolumeMountTypeTiFlashData)},
	}
	if image != "" {
		tmpl.Image = &image
	}
	if cfg := componentConfig(c, comp); cfg != "" {
		tmpl.Config = tidbcorev1.ConfigFile(cfg)
	}
	return &tidbcorev1.TiFlashGroup{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec: tidbcorev1.TiFlashGroupSpec{
			Cluster:  clusterRef(c),
			Replicas: replicasOrDefault(comp.Replicas, defaultTiFlashReplicas),
			Template: tidbcorev1.TiFlashTemplate{Spec: tmpl},
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
		Name:    dataVolumeName,
		Mounts:  []tidbcorev1.VolumeMount{{Type: mountType}},
		Storage: dataVolumeSize(s),
	}
	if s != nil && s.StorageClass != nil {
		v.StorageClassName = s.StorageClass
	}
	return v
}

func dataVolumeSize(s *corev1alpha1.Storage) resource.Quantity {
	if s == nil || s.Size.IsZero() {
		return defaultVolumeSize
	}
	return s.Size
}

func replicasOrDefault(r *int32, def int32) *int32 {
	if r != nil {
		return r
	}
	return &def
}
