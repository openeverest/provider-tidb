package provider

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// StatusTiDB reports the Instance phase and per-component readiness from the
// component groups, plus the reasons the operator gives for unhealthy instances.
func StatusTiDB(c *controller.Context) (controller.Status, error) {
	pd := &tidbcorev1.PDGroup{}
	if err := c.Get(pd, c.Name()); err != nil {
		return controller.Provisioning("Waiting for PD group to be created"), nil
	}
	tikv := &tidbcorev1.TiKVGroup{}
	if err := c.Get(tikv, c.Name()); err != nil {
		return controller.Provisioning("Waiting for TiKV group to be created"), nil
	}
	tidb := &tidbcorev1.TiDBGroup{}
	if err := c.Get(tidb, c.Name()); err != nil {
		return controller.Provisioning("Waiting for TiDB group to be created"), nil
	}

	groups := []groupRollout{
		{component: common.ComponentPD, replicas: pd.Spec.Replicas, version: pd.Spec.Template.Spec.Version, commonStatus: pd.Status.CommonStatus, groupStatus: pd.Status.GroupStatus},
		{component: common.ComponentTiKV, replicas: tikv.Spec.Replicas, version: tikv.Spec.Template.Spec.Version, commonStatus: tikv.Status.CommonStatus, groupStatus: tikv.Status.GroupStatus},
		{component: common.ComponentTiDB, replicas: tidb.Spec.Replicas, version: tidb.Spec.Template.Spec.Version, commonStatus: tidb.Status.CommonStatus, groupStatus: tidb.Status.GroupStatus},
	}
	problems, err := instanceProblems(c)
	if err != nil {
		return controller.Status{}, err
	}
	for i := range groups {
		groups[i].problem = problems[groups[i].component]
	}

	status := evaluateStatus(c.Instance().Status.Phase, groups)
	if status.Phase == corev1alpha1.InstancePhaseReady {
		status.ConnectionDetails = connectionDetails(c)
	}
	return status, nil
}

// groupRollout pairs a component group's desired state with what the operator
// last reported for it.
type groupRollout struct {
	component    string
	replicas     *int32
	version      string
	commonStatus tidbcorev1.CommonStatus
	groupStatus  tidbcorev1.GroupStatus
	// problem is the most severe issue the operator reports on one of the
	// group's instances, if any.
	problem componentProblem
}

type componentProblem struct {
	message string
	// fatal marks a state Kubernetes does not get out of without intervention.
	fatal bool
}

// converged mirrors the operator's IsGroupHealthyAndUpToDate minus observedGeneration,
// which churns while Context.Apply is a full Update (openeverest#3240).
func (g groupRollout) converged() bool {
	if g.replicas == nil || *g.replicas == 0 {
		return false
	}
	desired := *g.replicas
	s := g.groupStatus
	return s.Replicas == desired &&
		s.ReadyReplicas == desired &&
		s.UpdatedReplicas == desired &&
		s.CurrentReplicas == desired &&
		g.commonStatus.UpdateRevision == g.commonStatus.CurrentRevision &&
		s.Version == g.version
}

func (g groupRollout) desired() int32 {
	if g.replicas == nil {
		return 0
	}
	return *g.replicas
}

// summary reads e.g. "tikv (2/3 ready: <operator reason>)".
func (g groupRollout) summary() string {
	counts := fmt.Sprintf("%d/%d ready", g.groupStatus.ReadyReplicas, g.desired())
	reason := g.problem.message
	if reason == "" {
		if cond := meta.FindStatusCondition(g.commonStatus.Conditions, tidbcorev1.CondReady); cond != nil &&
			cond.Status == metav1.ConditionFalse {
			reason = cond.Message
		}
	}
	if reason == "" {
		return fmt.Sprintf("%s (%s)", g.component, counts)
	}
	return fmt.Sprintf("%s (%s: %s)", g.component, counts, reason)
}

func (g groupRollout) componentStatus() controller.ComponentStatus {
	state := "Ready"
	switch {
	case g.problem.fatal:
		state = "Error"
	case !g.converged():
		state = "InProgress"
	}
	return controller.ComponentStatus{
		Name:  g.component,
		Ready: g.groupStatus.ReadyReplicas,
		Total: g.desired(),
		State: state,
	}
}

// evaluateStatus picks the Instance phase: Failed if a component is stuck,
// Ready once every group converged, otherwise Provisioning on the first
// rollout and Updating on a cluster that was already serving.
func evaluateStatus(previous corev1alpha1.InstancePhase, groups []groupRollout) controller.Status {
	components := make([]controller.ComponentStatus, 0, len(groups))
	var pending []string
	var failure string
	for _, g := range groups {
		components = append(components, g.componentStatus())
		if g.problem.fatal && failure == "" {
			failure = fmt.Sprintf("%s is failing: %s", g.component, g.problem.message)
		}
		if !g.converged() {
			pending = append(pending, g.summary())
		}
	}

	var status controller.Status
	summary := strings.Join(pending, ", ")
	switch {
	case failure != "":
		status = controller.Failed(failure)
	case len(pending) == 0:
		status = controller.Ready()
	case previous == corev1alpha1.InstancePhaseReady || previous == corev1alpha1.InstancePhaseUpdating:
		status = controller.Updating("Rolling out changes to " + summary)
	default:
		status = controller.Provisioning("Waiting for " + summary)
	}
	status.Components = components
	return status
}

// instanceProblems reads the conditions the operator keeps on each PD, TiKV
// and TiDB instance and returns the most severe problem per component.
func instanceProblems(c *controller.Context) (map[string]componentProblem, error) {
	inCluster := client.MatchingLabels{tidbcorev1.LabelKeyCluster: c.Name()}
	problems := map[string]componentProblem{}

	pds := &tidbcorev1.PDList{}
	if err := c.List(pds, inCluster); err != nil {
		return nil, fmt.Errorf("listing pd instances: %w", err)
	}
	for i := range pds.Items {
		recordProblem(problems, common.ComponentPD, pds.Items[i].Name, pds.Items[i].Status.Conditions)
	}

	tikvs := &tidbcorev1.TiKVList{}
	if err := c.List(tikvs, inCluster); err != nil {
		return nil, fmt.Errorf("listing tikv instances: %w", err)
	}
	for i := range tikvs.Items {
		recordProblem(problems, common.ComponentTiKV, tikvs.Items[i].Name, tikvs.Items[i].Status.Conditions)
	}

	tidbs := &tidbcorev1.TiDBList{}
	if err := c.List(tidbs, inCluster); err != nil {
		return nil, fmt.Errorf("listing tidb instances: %w", err)
	}
	for i := range tidbs.Items {
		recordProblem(problems, common.ComponentTiDB, tidbs.Items[i].Name, tidbs.Items[i].Status.Conditions)
	}

	return problems, nil
}

// recordProblem keeps the first problem seen for a component, unless a later
// instance reports a fatal one.
func recordProblem(problems map[string]componentProblem, component, instance string, conds []metav1.Condition) {
	problem, ok := instanceProblem(instance, conds)
	if !ok {
		return
	}
	if existing, seen := problems[component]; seen && (existing.fatal || !problem.fatal) {
		return
	}
	problems[component] = problem
}

func instanceProblem(instance string, conds []metav1.Condition) (componentProblem, bool) {
	if cond := meta.FindStatusCondition(conds, tidbcorev1.CondRunning); cond != nil && cond.Status == metav1.ConditionFalse {
		return componentProblem{
			message: fmt.Sprintf("instance %s: %s", instance, cond.Message),
			fatal:   isStuckContainer(cond.Message),
		}, true
	}
	if cond := meta.FindStatusCondition(conds, tidbcorev1.CondReady); cond != nil && cond.Status == metav1.ConditionFalse {
		return componentProblem{message: fmt.Sprintf("instance %s: %s", instance, cond.Message)}, true
	}
	return componentProblem{}, false
}

// stuckWaitingReasons are container waiting states that retrying alone does
// not fix. CrashLoopBackOff only counts once kubelet's back-off hits its
// ceiling, so the restarts TiDB goes through while PD/TiKV bootstrap don't
// flag the Instance as Failed.
var stuckWaitingReasons = []string{
	"ImagePullBackOff",
	"ErrImageNeverPull",
	"InvalidImageName",
	"CreateContainerConfigError",
}

const maxCrashLoopBackOff = "back-off 5m0s"

func isStuckContainer(message string) bool {
	if strings.Contains(message, "reason: CrashLoopBackOff") {
		return strings.Contains(message, maxCrashLoopBackOff)
	}
	for _, reason := range stuckWaitingReasons {
		if strings.Contains(message, "reason: "+reason) {
			return true
		}
	}
	return false
}

// connectionDetails points at the TiDB internal (SQL) service, which the
// operator names "<group>-tidb" and exposes on the MySQL client port.
func connectionDetails(c *controller.Context) controller.ConnectionDetails {
	return controller.ConnectionDetails{
		Type:     "mysql",
		Provider: common.ProviderName,
		Host:     fmt.Sprintf("%s-tidb.%s", c.Name(), c.Namespace()),
		Port:     strconv.Itoa(tidbcorev1.DefaultTiDBPortClient),
		Username: rootUser,
		Password: readRootPassword(c),
	}
}
