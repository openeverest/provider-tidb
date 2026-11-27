package provider

import (
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"
)

// sccKind is served only by OpenShift.
var sccKind = schema.GroupKind{Group: "security.openshift.io", Kind: "SecurityContextConstraints"}

// openShiftByMapper caches the answer per RESTMapper: a miss makes the lazy
// mapper re-run discovery, which would otherwise happen on every reconcile
// outside OpenShift.
var openShiftByMapper sync.Map

// isOpenShift reports whether the cluster is OpenShift.
func isOpenShift(k8s client.Client) (bool, error) {
	mapper := k8s.RESTMapper()
	if v, ok := openShiftByMapper.Load(mapper); ok {
		return v.(bool), nil
	}
	_, err := mapper.RESTMapping(sccKind, "v1")
	switch {
	case err == nil:
	case meta.IsNoMatchError(err):
	default:
		return false, fmt.Errorf("discover %s: %w", sccKind, err)
	}
	openShiftByMapper.Store(mapper, err == nil)
	return err == nil, nil
}

// addOpenShiftSlowLog moves TiDB's slow log to the operator's emptyDir. TiDB
// writes it to tidb-slow.log in its working directory, /, which only root can
// write, and OpenShift's restricted-v2 SCC runs the pod under a namespace UID.
// Only on OpenShift, where TiDB can't start otherwise: elsewhere the change
// would restart every TiDB pod.
func addOpenShiftSlowLog(c *controller.Context, group *tidbcorev1.TiDBGroup) error {
	openShift, err := isOpenShift(c.Client())
	if err != nil {
		return err
	}
	if openShift {
		group.Spec.Template.Spec.SlowLog = &tidbcorev1.TiDBSlowLog{}
	}
	return nil
}
