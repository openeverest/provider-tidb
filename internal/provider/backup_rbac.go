package provider

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-tidb/internal/common"
)

// backupManagerName names the ServiceAccount, Role and RoleBinding the BR
// backup, restore and cleanup jobs run as. TiDB Operator defaults the jobs to
// "tidb-backup-manager" but creates no such account; the provider sets its own
// on every BR Backup and Restore so it never depends on, or changes, one the
// user may manage.
const backupManagerName = "openeverest-tidb-backup-manager"

// backupManagerRules is what tidb-backup-manager does with the API: read the
// BR Backup or Restore it runs for, update its status and record events.
var backupManagerRules = []rbacv1.PolicyRule{
	{
		APIGroups: []string{""},
		Resources: []string{"events"},
		Verbs:     []string{"create", "patch"},
	},
	{
		APIGroups: []string{"br.pingcap.com"},
		Resources: []string{"backups", "backups/status", "restores", "restores/status"},
		Verbs:     []string{"get", "list", "watch", "update"},
	},
}

// ensureBackupManagerRBAC applies the account the BR jobs run as in namespace.
// Every Instance in the namespace shares it, so it has no owner and stays with
// the namespace.
func ensureBackupManagerRBAC(c *controller.Context, namespace string) error {
	meta := metav1.ObjectMeta{
		Name:      backupManagerName,
		Namespace: namespace,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "everest",
			controller.ProviderLabel:       common.ProviderName,
		},
	}
	objs := []client.Object{
		&corev1.ServiceAccount{ObjectMeta: meta},
		&rbacv1.Role{ObjectMeta: meta, Rules: backupManagerRules},
		&rbacv1.RoleBinding{
			ObjectMeta: meta,
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: backupManagerName, Namespace: namespace}},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: backupManagerName},
		},
	}
	for _, obj := range objs {
		if err := applyServerSide(c, obj); err != nil {
			return fmt.Errorf("apply BR job %T %s: %w", obj, backupManagerName, err)
		}
	}
	return nil
}

// applyServerSide server-side applies obj as the provider, claiming only the
// fields it sets.
func applyServerSide(c *controller.Context, obj client.Object) error {
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
	return c.Client().Apply(c.Context(),
		client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: raw}),
		client.FieldOwner("provider-"+common.ProviderName),
		client.ForceOwnership,
	)
}
