package provider

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/structured-merge-diff/v6/value"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
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
// through the operator's ordered Cluster teardown (see applyOwnedByCluster).
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

	// The Cluster is owned by the Instance (via c.Apply). Server-side apply only
	// manages the fields this provider sets, so the operator's own fields —
	// including its finalizer — stay intact and deleting the Instance triggers
	// the operator's ordered Cluster teardown.
	cluster, err := buildCluster(c)
	if err != nil {
		return err
	}
	if err := c.Apply(cluster); err != nil {
		return err
	}
	// Re-read the Cluster so its UID is available for the group owner references.
	owner := &tidbcorev1.Cluster{}
	if err := c.Get(owner, c.Name()); err != nil {
		return fmt.Errorf("get cluster for ownership: %w", err)
	}

	pd := comps[common.ComponentPD]
	if err := applyOwnedByCluster(c, owner, buildPDGroup(c, pd, resolveImage(providerSpec, common.ComponentPD, pd))); err != nil {
		return err
	}

	tikv := comps[common.ComponentTiKV]
	if err := applyOwnedByCluster(c, owner, buildTiKVGroup(c, tikv, resolveImage(providerSpec, common.ComponentTiKV, tikv))); err != nil {
		return err
	}

	tidb := comps[common.ComponentTiDB]
	if err := applyOwnedByCluster(c, owner, buildTiDBGroup(c, tidb, resolveImage(providerSpec, common.ComponentTiDB, tidb))); err != nil {
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
		return applyOwnedByCluster(c, owner, buildTiFlashGroup(c, tiflash, resolveImage(providerSpec, common.ComponentTiFlash, tiflash)))
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

// applyOwnedByCluster server-side applies obj with the Cluster as its controller
// owner. The runtime's c.Apply always owns objects by the Instance, but the
// component groups must be owned by the Cluster: deleting the Instance then
// garbage-collects only the Cluster, whose operator finalizer tears the groups
// down in order. Owning the groups by the Instance would garbage-collect them
// all at once, killing PD mid-eviction and deadlocking TiKV on its "leaders are
// not all evicted" finalizer.
//
// It mirrors the runtime's server-side apply (drop status, nulls and implicit
// empty structs, force ownership) so it claims only the fields the provider
// sets and leaves the operator's fields — including its finalizer — untouched.
func applyOwnedByCluster(c *controller.Context, cluster, obj client.Object) error {
	if err := controllerutil.SetControllerReference(cluster, obj, c.Client().Scheme()); err != nil {
		return fmt.Errorf("set cluster owner: %w", err)
	}
	gvk, err := apiutil.GVKForObject(obj, c.Client().Scheme())
	if err != nil {
		return fmt.Errorf("resolve GVK for apply: %w", err)
	}
	obj.GetObjectKind().SetGroupVersionKind(gvk)

	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return fmt.Errorf("build apply configuration: %w", err)
	}
	delete(raw, "status")
	pruneNulls(raw)
	pruneEmptyStructs(reflect.ValueOf(obj), raw)
	return c.Client().Apply(c.Context(),
		client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: raw}),
		client.FieldOwner("provider-"+common.ProviderName),
		client.ForceOwnership,
	)
}

// pruneNulls drops null values so server-side apply does not claim unset fields,
// matching the runtime's c.Apply.
func pruneNulls(v any) {
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			if child == nil {
				delete(val, k)
				continue
			}
			pruneNulls(child)
		}
	case []any:
		for _, child := range val {
			pruneNulls(child)
		}
	}
}

// pruneEmptyStructs drops the {} ToUnstructured emits for empty non-pointer
// omitempty structs (e.g. a group's resources once CPU and memory are cleared),
// matching the runtime's c.Apply (openeverest#3282). Otherwise SSA prunes the
// children, the API server stores null, and the CRD rejects the whole apply.
// A non-nil pointer keeps its {} (emptyDir: {} selects a volume type).
func pruneEmptyStructs(v reflect.Value, raw any) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	// Custom marshalers (Quantity, Time, RawExtension) decide their own shape.
	if !v.IsValid() || value.TypeReflectEntryOf(v.Type()).CanConvertToUnstructured() {
		return
	}

	switch r := raw.(type) {
	case map[string]any:
		if v.Kind() == reflect.Struct {
			pruneStructFields(v, r)
			return
		}
		pruneMapValues(v, r)
	case []any:
		if (v.Kind() == reflect.Slice || v.Kind() == reflect.Array) && v.Len() == len(r) {
			for i := range r {
				pruneEmptyStructs(v.Index(i), r[i])
			}
		}
	}
}

func pruneStructFields(v reflect.Value, m map[string]any) {
	for i := range v.NumField() {
		field := v.Type().Field(i)
		name, omitempty := jsonField(field)
		if name == "-" {
			continue
		}
		if name == "" {
			pruneEmptyStructs(v.Field(i), m)
			continue
		}
		child, ok := m[name]
		if !ok {
			continue
		}
		pruneEmptyStructs(v.Field(i), child)
		if childMap, isMap := child.(map[string]any); isMap && len(childMap) == 0 && omitempty && isPlainStruct(field.Type) {
			delete(m, name)
		}
	}
}

func pruneMapValues(v reflect.Value, m map[string]any) {
	if v.Kind() != reflect.Map || v.Type().Key().Kind() != reflect.String {
		return
	}
	for it := v.MapRange(); it.Next(); {
		if child, ok := m[it.Key().String()]; ok {
			pruneEmptyStructs(it.Value(), child)
		}
	}
}

func isPlainStruct(t reflect.Type) bool {
	return t.Kind() == reflect.Struct && !value.TypeReflectEntryOf(t).CanConvertToUnstructured()
}

// jsonField returns a field's key as runtime.DefaultUnstructuredConverter
// names it ("" when inlined) and whether it is tagged omitempty.
func jsonField(f reflect.StructField) (string, bool) {
	name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" && !f.Anonymous {
		name = f.Name
	}
	return name, slices.Contains(strings.Split(opts, ","), "omitempty")
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
		Overlay:   podOverlay(c, common.ComponentPD, comp.SchedulingPolicy),
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
		Overlay:   podOverlay(c, common.ComponentTiKV, comp.SchedulingPolicy),
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
		Overlay:   podOverlay(c, common.ComponentTiDB, comp.SchedulingPolicy),
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
		Overlay:   podOverlay(c, common.ComponentTiFlash, comp.SchedulingPolicy),
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

// podOverlay labels a component's pods so the runtime counts them into the
// Instance's status.components, and places them per the component's scheduling
// policy. The operator copies only its own labels from the group template to
// the pods, so both go through the pod overlay. Without a policy the pods keep
// the operator's placement: no affinity and the scheduler's built-in spreading.
func podOverlay(c *controller.Context, component string, policy *commonv1alpha1.SchedulingPolicy) *tidbcorev1.Overlay {
	labels := c.PodLabels(component)
	pod := &tidbcorev1.PodOverlay{ObjectMeta: tidbcorev1.ObjectMeta{Labels: labels}}
	if policy == nil {
		return &tidbcorev1.Overlay{Pod: pod}
	}
	spec := corev1.PodSpec{
		SchedulerName:             policy.SchedulerName,
		NodeSelector:              policy.NodeSelector,
		Affinity:                  policy.Affinity,
		Tolerations:               policy.Tolerations,
		TopologySpreadConstraints: controller.TopologySpreadConstraints(policy, labels),
	}
	// An empty spec would still be applied as {} and roll the pods for nothing.
	if !equality.Semantic.DeepEqual(spec, corev1.PodSpec{}) {
		pod.Spec = &spec
	}
	return &tidbcorev1.Overlay{Pod: pod}
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
