package provider

import (
	"fmt"
	"net"
	"net/url"
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

// Reasons of what the provider reports about clients' access.
const (
	reasonNoSQLServerReady = "NoSQLServerReady"
	reasonNoPDQuorum       = "NoPDQuorum"
	reasonNoTiKVReady      = "NoTiKVReady"
	reasonTiKVNodesDown    = "TiKVNodesDown"
)

// reasonRollingOutVersion is the reason of the Upgrading operation.
const reasonRollingOutVersion = "RollingOutVersion"

// StatusTiDB reports what the provider observes about the TiDB cluster:
// whether every component group runs what the Instance asks for, whether
// clients can connect, what is wrong and which long operation is running, and
// how many pods each component will have. The runtime derives the Instance's
// phase and conditions from these facts.
func StatusTiDB(c *controller.Context) (controller.Status, error) {
	// While an initial-seeding restore (.spec.dataSource) is in flight the
	// Instance is restoring, and only converges once the data is restored.
	if ds := c.GetDataSourceStatus(); ds != nil && !ds.Done {
		return withDesiredReplicas(c, controller.Restoring(ds.Message)), nil
	}

	pd := &tidbcorev1.PDGroup{}
	if err := c.Get(pd, c.Name()); err != nil {
		return withDesiredReplicas(c, controller.Progressing("Waiting for PD group to be created")), nil
	}
	tikv := &tidbcorev1.TiKVGroup{}
	if err := c.Get(tikv, c.Name()); err != nil {
		return withDesiredReplicas(c, controller.Progressing("Waiting for TiKV group to be created")), nil
	}
	tidb := &tidbcorev1.TiDBGroup{}
	if err := c.Get(tidb, c.Name()); err != nil {
		return withDesiredReplicas(c, controller.Progressing("Waiting for TiDB group to be created")), nil
	}

	groups := []groupRollout{
		{component: common.ComponentPD, replicas: pd.Spec.Replicas, version: pd.Spec.Template.Spec.Version, commonStatus: pd.Status.CommonStatus, groupStatus: pd.Status.GroupStatus},
		{component: common.ComponentTiKV, replicas: tikv.Spec.Replicas, version: tikv.Spec.Template.Spec.Version, commonStatus: tikv.Status.CommonStatus, groupStatus: tikv.Status.GroupStatus},
		{component: common.ComponentTiDB, replicas: tidb.Spec.Replicas, version: tidb.Spec.Template.Spec.Version, commonStatus: tidb.Status.CommonStatus, groupStatus: tidb.Status.GroupStatus},
	}
	tiflash := &tidbcorev1.TiFlashGroup{}
	found, err := c.Exists(tiflash, c.Name())
	if err != nil {
		return controller.Status{}, fmt.Errorf("get tiflash group: %w", err)
	}
	tiflashSpec, tiflashEnabled := enabledTiFlash(c.Instance().Spec.Components)
	switch {
	case tiflashEnabled && !found:
		// Not an early return: the cache may not show the group just applied.
		groups = append(groups, groupRollout{
			component: common.ComponentTiFlash, replicas: replicasOrDefault(tiflashSpec.Replicas, defaultTiFlashReplicas),
		})
	case found:
		groups = append(groups, groupRollout{
			component: common.ComponentTiFlash, replicas: tiflash.Spec.Replicas, version: tiflash.Spec.Template.Spec.Version,
			commonStatus: tiflash.Status.CommonStatus, groupStatus: tiflash.Status.GroupStatus, removing: !tiflashEnabled,
		})
	}
	tiproxy := &tidbcorev1.TiProxyGroup{}
	found, err = c.Exists(tiproxy, c.Name())
	if err != nil {
		return controller.Status{}, fmt.Errorf("get tiproxy group: %w", err)
	}
	tiproxySpec, tiproxyEnabled := enabledTiProxy(c.Instance().Spec.Components)
	switch {
	case tiproxyEnabled && !found:
		groups = append(groups, groupRollout{
			component: common.ComponentTiProxy, replicas: replicasOrDefault(tiproxySpec.Replicas, defaultTiProxyReplicas),
		})
	case found:
		groups = append(groups, groupRollout{
			component: common.ComponentTiProxy, replicas: tiproxy.Spec.Replicas, version: tiproxy.Spec.Template.Spec.Version,
			commonStatus: tiproxy.Status.CommonStatus, groupStatus: tiproxy.Status.GroupStatus, removing: !tiproxyEnabled,
		})
	}
	problems, err := instanceProblems(c)
	if err != nil {
		return controller.Status{}, err
	}
	for i := range groups {
		groups[i].problem = problems[groups[i].component]
	}

	status := evaluateStatus(groups, tiproxyEnabled)
	// Clients are told where to connect as soon as they can, not only once
	// the cluster has converged, and the details follow a change still
	// rolling out.
	if status.Converged || status.Available == controller.Available {
		status.ConnectionDetails = connectionDetails(c)
	}
	return withDesiredReplicas(c, status), nil
}

// withDesiredReplicas reports how many pods each component of the Instance
// will have once the current spec has converged, with Sync's defaults.
func withDesiredReplicas(c *controller.Context, status controller.Status) controller.Status {
	defaults := map[string]int32{
		common.ComponentPD:      defaultPDReplicas,
		common.ComponentTiKV:    defaultTiKVReplicas,
		common.ComponentTiDB:    defaultTiDBReplicas,
		common.ComponentTiFlash: defaultTiFlashReplicas,
		common.ComponentTiProxy: defaultTiProxyReplicas,
	}
	for name, spec := range c.Instance().Spec.Components {
		def, known := defaults[name]
		if !known {
			continue
		}
		status = status.WithDesiredReplicas(name, *replicasOrDefault(spec.Replicas, def))
	}
	return status
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
	// removing marks a group the Instance no longer wants, still being torn down.
	removing bool
}

type componentProblem struct {
	message string
	// reason names a state Kubernetes does not get out of by retrying alone,
	// such as CrashLoopBackOff at its longest back-off; empty otherwise.
	reason string
}

// converged mirrors the operator's IsGroupHealthyAndUpToDate minus observedGeneration,
// which churns while Context.Apply is a full Update (openeverest#3240).
func (g groupRollout) converged() bool {
	if g.removing || g.replicas == nil || *g.replicas == 0 {
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
	if g.removing || g.replicas == nil {
		return 0
	}
	return *g.replicas
}

// upgrading reports whether the group rolls out another version than the one
// it runs. The operator records the version once the group has rolled it out.
func (g groupRollout) upgrading() bool {
	return !g.removing && g.version != "" && g.groupStatus.Version != "" && g.groupStatus.Version != g.version
}

// summary reads e.g. "tikv (2/3 ready: <operator reason>)" or "tiflash (removing)".
func (g groupRollout) summary() string {
	counts := fmt.Sprintf("%d/%d ready", g.groupStatus.ReadyReplicas, g.desired())
	if g.removing {
		counts = "removing"
	}
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

// evaluateStatus turns the component groups into the facts the provider
// reports: converged once every group runs what the Instance asks for, the
// problems the operator reports, a version rollout, and whether clients can
// connect. tiproxy says whether clients connect through TiProxy.
func evaluateStatus(groups []groupRollout, tiproxy bool) controller.Status {
	var pending, upgrading []string
	var status controller.Status
	for _, g := range groups {
		if g.problem.reason != "" {
			status = status.WithProblem(g.problem.reason, fmt.Sprintf("%s is failing: %s", g.component, g.problem.message))
		}
		if !g.converged() {
			pending = append(pending, g.summary())
		}
		if g.upgrading() {
			upgrading = append(upgrading, fmt.Sprintf("%s to %s", g.component, g.version))
		}
	}
	if len(upgrading) > 0 {
		status = status.WithOperation(corev1alpha1.ConditionUpgrading, reasonRollingOutVersion,
			"Rolling out "+strings.Join(upgrading, ", "))
	}
	if len(pending) == 0 {
		status.Converged = true
	} else {
		status.Message = "Waiting for " + strings.Join(pending, ", ")
	}
	return withAvailability(status, groups, tiproxy)
}

// withAvailability reports whether clients can connect, from the ready pods
// of each group, so it stays True through a rolling restart: PD keeps quorum,
// a TiKV node at a time is down at most, and the SQL servers clients connect
// to (TiProxy, or TiDB) have a ready pod. With more TiKV nodes down, whether
// every Region still has a majority of its replicas depends on where PD put
// them, so the answer is unknown.
func withAvailability(status controller.Status, groups []groupRollout, tiproxy bool) controller.Status {
	byComponent := map[string]groupRollout{}
	for _, g := range groups {
		byComponent[g.component] = g
	}
	sql := byComponent[common.ComponentTiDB]
	if tiproxy {
		sql = byComponent[common.ComponentTiProxy]
	}
	pd, tikv := byComponent[common.ComponentPD], byComponent[common.ComponentTiKV]
	pdReady, tikvReady := pd.groupStatus.ReadyReplicas, tikv.groupStatus.ReadyReplicas
	switch {
	case sql.groupStatus.ReadyReplicas == 0:
		return status.WithUnavailable(reasonNoSQLServerReady, fmt.Sprintf("No %s pod is ready", sql.component))
	case pdReady <= pd.desired()/2:
		return status.WithUnavailable(reasonNoPDQuorum,
			fmt.Sprintf("PD has no quorum: %d of %d pods ready", pdReady, pd.desired()))
	case tikvReady == 0:
		return status.WithUnavailable(reasonNoTiKVReady, "No TiKV pod is ready")
	case tikvReady < tikv.desired()-1 || (tikvReady < tikv.desired() && tikv.desired() < 3):
		status.Available = controller.AvailabilityUnknown
		status.AvailableReason = reasonTiKVNodesDown
		status.AvailableMessage = fmt.Sprintf("%d of %d TiKV pods ready: some data may have no majority of its replicas",
			tikvReady, tikv.desired())
		return status
	}
	return status.WithAvailable(controller.Available)
}

// instanceProblems reads the conditions the operator keeps on each PD, TiKV,
// TiDB, TiFlash and TiProxy instance and returns the most severe problem per component.
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

	tiflashes := &tidbcorev1.TiFlashList{}
	if err := c.List(tiflashes, inCluster); err != nil {
		return nil, fmt.Errorf("listing tiflash instances: %w", err)
	}
	for i := range tiflashes.Items {
		recordProblem(problems, common.ComponentTiFlash, tiflashes.Items[i].Name, tiflashes.Items[i].Status.Conditions)
	}

	tiproxies := &tidbcorev1.TiProxyList{}
	if err := c.List(tiproxies, inCluster); err != nil {
		return nil, fmt.Errorf("listing tiproxy instances: %w", err)
	}
	for i := range tiproxies.Items {
		recordProblem(problems, common.ComponentTiProxy, tiproxies.Items[i].Name, tiproxies.Items[i].Status.Conditions)
	}

	return problems, nil
}

// recordProblem keeps the first problem seen for a component, unless a later
// instance reports one retrying won't fix.
func recordProblem(problems map[string]componentProblem, component, instance string, conds []metav1.Condition) {
	problem, ok := instanceProblem(instance, conds)
	if !ok {
		return
	}
	if existing, seen := problems[component]; seen && (existing.reason != "" || problem.reason == "") {
		return
	}
	problems[component] = problem
}

func instanceProblem(instance string, conds []metav1.Condition) (componentProblem, bool) {
	if cond := meta.FindStatusCondition(conds, tidbcorev1.CondRunning); cond != nil && cond.Status == metav1.ConditionFalse {
		return componentProblem{
			message: fmt.Sprintf("instance %s: %s", instance, cond.Message),
			reason:  stuckContainerReason(cond.Message),
		}, true
	}
	if cond := meta.FindStatusCondition(conds, tidbcorev1.CondReady); cond != nil && cond.Status == metav1.ConditionFalse {
		return componentProblem{message: fmt.Sprintf("instance %s: %s", instance, cond.Message)}, true
	}
	return componentProblem{}, false
}

// stuckWaitingReasons are container waiting states that retrying alone does
// not fix. CrashLoopBackOff only counts once kubelet's back-off hits its
// ceiling, so the restarts TiDB goes through while PD/TiKV bootstrap aren't
// reported as a problem.
var stuckWaitingReasons = []string{
	"ImagePullBackOff",
	"ErrImageNeverPull",
	"InvalidImageName",
	"CreateContainerConfigError",
}

const maxCrashLoopBackOff = "back-off 5m0s"

// stuckContainerReason returns the waiting reason of a container that is
// stuck, as the operator words it in an instance's Running condition, or "".
func stuckContainerReason(message string) string {
	if strings.Contains(message, "reason: CrashLoopBackOff") {
		if strings.Contains(message, maxCrashLoopBackOff) {
			return "CrashLoopBackOff"
		}
		return ""
	}
	for _, reason := range stuckWaitingReasons {
		if strings.Contains(message, "reason: "+reason) {
			return reason
		}
	}
	return ""
}

// connectionDetails points at the internal (SQL) service clients should use:
// TiProxy's "<group>-tiproxy" on its client port while TiProxy runs, otherwise
// TiDB's "<group>-tidb" on the MySQL client port.
func connectionDetails(c *controller.Context) controller.ConnectionDetails {
	host := fmt.Sprintf("%s-tidb.%s", c.Name(), c.Namespace())
	port := tidbcorev1.DefaultTiDBPortClient
	if _, ok := enabledTiProxy(c.Instance().Spec.Components); ok {
		host = fmt.Sprintf("%s-tiproxy.%s", c.Name(), c.Namespace())
		port = tidbcorev1.DefaultTiProxyPortClient
	}
	details := controller.ConnectionDetails{
		Type:     "mysql",
		Provider: common.ProviderName,
		Host:     host,
		Port:     strconv.Itoa(port),
		Username: rootUser,
		Password: readRootPassword(c),
	}
	// With client TLS the host is in TiDB's (or TiProxy's) certificate, signed
	// by the same CA, so clients can verify it against the CA
	// (e.g. mysql --ssl-mode=VERIFY_IDENTITY --ssl-ca=ca.crt).
	clientTLS := tlsSettings(c).Client
	if clientTLS {
		if ca, err := readClientCA(c); err == nil && len(ca) > 0 {
			details.AdditionalProperties = map[string]string{tlsKeyCA: string(ca)}
		}
	}
	details.URI = connectionURI(details, clientTLS)
	return details
}

// connectionURI is the endpoint as a mysql:// URI with Go driver TLS names,
// as the other OpenEverest MySQL providers publish it: tls=true verifies the
// server against ca.crt, tls=false connects without TLS.
func connectionURI(details controller.ConnectionDetails, clientTLS bool) string {
	uri := url.URL{
		Scheme:   "mysql",
		User:     url.UserPassword(details.Username, details.Password),
		Host:     net.JoinHostPort(details.Host, details.Port),
		Path:     "/",
		RawQuery: "tls=" + strconv.FormatBool(clientTLS),
	}
	return uri.String()
}
