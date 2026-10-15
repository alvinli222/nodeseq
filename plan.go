// Package nodeseq computes node-level startup ordering for Kubernetes pods.
//
// Pods are organised into ordered groups (each selected by a label selector). For every participating
// node, nodeseq computes how many consecutive groups (starting at group 0) are satisfied on that node,
// and which scheduling-gated pods may be released:
//
//   - Pinned pods (pods whose node is known before scheduling, such as DaemonSet pods, which carry a
//     required node affinity on metadata.name) in group k are released when at least k groups are
//     satisfied on their node. Each node progresses independently.
//   - Unpinned pods (e.g. Deployment pods) are returned with ReasonUnpinned; the caller decides how to
//     steer them (node labels + affinity, taints + tolerations, ...).
//
// A group is satisfied on a node when every active pod of the group targeting the node is Ready (for at
// least the group's Wait), and every DaemonSet in the group that is expected on the node already has a
// pod there. If a group has no pods on a node and no DaemonSet is expected there, its Policy decides.
//
// The package is pure: it performs no API calls. Callers list objects (typically from an informer
// cache), call Compute, and apply the result (remove gates, update node labels, report status).
package nodeseq

import (
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Policy controls how a group is evaluated on a node that has no pods of the group.
type Policy string

const (
	// PolicySkipIfNotScheduled treats the group as satisfied on a node when no pod of the group
	// targets the node and no DaemonSet of the group is expected to run there. This is the default.
	PolicySkipIfNotScheduled Policy = "SkipIfNotScheduled"
	// PolicyRequireOnNode requires at least one Ready pod of the group on the node.
	PolicyRequireOnNode Policy = "RequireOnNode"
)

// Release reasons.
const (
	ReasonSatisfied        = "satisfied"
	ReasonBypass           = "bypass"
	ReasonUnpinned         = "unpinned"
	ReasonNotParticipating = "node-not-participating"
	ReasonNodeMissing      = "node-missing"
)

// Group is one ordered stage of a sequence.
type Group struct {
	// Name is used in messages. Optional.
	Name string
	// Selector selects the pods (and, through their pod templates, the DaemonSets) of the group. Required.
	Selector labels.Selector
	// Wait is how long every pod of the group must have been Ready before the group is satisfied.
	Wait time.Duration
	// Policy defaults to PolicySkipIfNotScheduled.
	Policy Policy
}

// Input is everything Compute needs. Slices are not modified.
type Input struct {
	// Groups in order. Required.
	Groups []Group
	// Namespace restricts which pods and DaemonSets are considered ("" = all namespaces).
	Namespace string
	// Gate is the scheduling gate name managed by the caller. Only pods holding it are released.
	Gate string
	// Nodes is the full node list.
	Nodes []corev1.Node
	// NodeSelector restricts participating nodes (nil = all nodes).
	NodeSelector labels.Selector
	// Pods and DaemonSets to consider.
	Pods       []corev1.Pod
	DaemonSets []appsv1.DaemonSet
	// Now is the evaluation time.
	Now time.Time
	// GateTimeout, if > 0, reports pinned pods gated longer than this in Plan.TimedOut and marks their
	// node Failed. Timed-out pods are NOT released; the caller decides whether to fail open.
	GateTimeout time.Duration
	// IgnoreTaintKeys are taints (usually owned by the caller) ignored when deciding whether a
	// DaemonSet is expected on a node.
	IgnoreTaintKeys []string
	// Bypass, if set, releases matching gated pods immediately with ReasonBypass.
	Bypass func(*corev1.Pod) bool
}

// NodeState is the computed state of one node.
type NodeState struct {
	Name          string
	Participating bool
	// Satisfied is the number of consecutive groups (from group 0) satisfied on the node; it is also
	// the index of the group the node is waiting on. Equals the number of groups when complete.
	Satisfied int
	// Message explains what a non-complete node is waiting on.
	Message string
	// Failed is set when a pinned pod on the node exceeded GateTimeout.
	Failed bool
}

// Release is a gated pod that may proceed.
type Release struct {
	Pod   *corev1.Pod
	Group int
	// Node is the pod's target node ("" for unpinned pods).
	Node   string
	Reason string
}

// TimedOut is a pinned pod gated for longer than Input.GateTimeout.
type TimedOut struct {
	Pod   *corev1.Pod
	Node  string
	Group int
}

// Plan is the result of Compute.
type Plan struct {
	NumGroups int
	// Nodes contains every node, sorted by name.
	Nodes []NodeState
	// Release lists gated pods that may proceed.
	Release []Release
	// GatedPods counts member pods that remain gated after Release is applied.
	GatedPods int
	// TimedOut lists pinned pods gated longer than GateTimeout (they remain gated).
	TimedOut []TimedOut
	// DaemonSets lists, per group, the DaemonSets whose pod template the group selects.
	DaemonSets [][]*appsv1.DaemonSet
	// RequeueAfter is the earliest time-based re-evaluation (Wait or GateTimeout), 0 if none.
	RequeueAfter time.Duration

	nodeIndex map[string]int
}

// Node returns the state of the named node.
func (p *Plan) Node(name string) (NodeState, bool) {
	i, ok := p.nodeIndex[name]
	if !ok {
		return NodeState{}, false
	}
	return p.Nodes[i], true
}

// GroupIndex returns the index of the first group whose selector matches the pod's labels, or -1.
func GroupIndex(groups []Group, pod *corev1.Pod) int {
	for i, g := range groups {
		if g.Selector != nil && g.Selector.Matches(labels.Set(pod.Labels)) {
			return i
		}
	}
	return -1
}

type member struct {
	pod   *corev1.Pod
	group int
	node  string
}

// Compute evaluates every node and every gated member pod.
func Compute(in Input) (*Plan, error) {
	numGroups := len(in.Groups)
	for i, g := range in.Groups {
		if g.Selector == nil {
			return nil, fmt.Errorf("group %d (%s) has no selector", i, g.Name)
		}
		if g.Policy != "" && g.Policy != PolicySkipIfNotScheduled && g.Policy != PolicyRequireOnNode {
			return nil, fmt.Errorf("group %d (%s) has unknown policy %q", i, g.Name, g.Policy)
		}
	}
	nodeSel := in.NodeSelector
	if nodeSel == nil {
		nodeSel = labels.Everything()
	}

	plan := &Plan{NumGroups: numGroups, nodeIndex: map[string]int{}}
	var dsInNS []appsv1.DaemonSet
	for i := range in.DaemonSets {
		if in.Namespace == "" || in.DaemonSets[i].Namespace == in.Namespace {
			dsInNS = append(dsInNS, in.DaemonSets[i])
		}
	}
	plan.DaemonSets = make([][]*appsv1.DaemonSet, numGroups)
	for i, g := range in.Groups {
		plan.DaemonSets[i] = DaemonSetsForSelector(dsInNS, g.Selector)
	}

	var members []member
	podsByNodeGroup := map[string][][]*corev1.Pod{}
	for i := range in.Pods {
		pod := &in.Pods[i]
		if in.Namespace != "" && pod.Namespace != in.Namespace {
			continue
		}
		idx := GroupIndex(in.Groups, pod)
		if idx < 0 || !IsActive(pod) {
			continue
		}
		m := member{pod: pod, group: idx, node: TargetNode(pod)}
		members = append(members, m)
		if m.node != "" {
			if podsByNodeGroup[m.node] == nil {
				podsByNodeGroup[m.node] = make([][]*corev1.Pod, numGroups)
			}
			podsByNodeGroup[m.node][idx] = append(podsByNodeGroup[m.node][idx], pod)
		}
	}

	nodes := append([]corev1.Node(nil), in.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	for i := range nodes {
		node := &nodes[i]
		st := NodeState{Name: node.Name}
		st.Participating = node.DeletionTimestamp == nil && nodeSel.Matches(labels.Set(node.Labels))
		if st.Participating {
			st.Satisfied = numGroups
			for j := 0; j < numGroups; j++ {
				var podsHere []*corev1.Pod
				if byGroup := podsByNodeGroup[node.Name]; byGroup != nil {
					podsHere = byGroup[j]
				}
				ok, remaining, msg := groupSatisfiedOnNode(in.Groups[j], j, node, podsHere, plan.DaemonSets[j], in.Now, in.IgnoreTaintKeys)
				plan.requeue(remaining)
				if !ok {
					st.Satisfied = j
					st.Message = msg
					break
				}
			}
		}
		plan.nodeIndex[node.Name] = len(plan.Nodes)
		plan.Nodes = append(plan.Nodes, st)
	}

	for _, m := range members {
		if in.Gate == "" || !HasGate(m.pod, in.Gate) {
			continue
		}
		rel := Release{Pod: m.pod, Group: m.group, Node: m.node}
		if in.Bypass != nil && in.Bypass(m.pod) {
			rel.Reason = ReasonBypass
			plan.Release = append(plan.Release, rel)
			continue
		}
		if m.node == "" {
			rel.Reason = ReasonUnpinned
			plan.Release = append(plan.Release, rel)
			continue
		}
		si, exists := plan.nodeIndex[m.node]
		switch {
		case !exists:
			rel.Reason = ReasonNodeMissing
		case !plan.Nodes[si].Participating:
			rel.Reason = ReasonNotParticipating
		case plan.Nodes[si].Satisfied >= m.group:
			rel.Reason = ReasonSatisfied
		default:
			plan.GatedPods++
			if in.GateTimeout > 0 {
				deadline := m.pod.CreationTimestamp.Add(in.GateTimeout)
				if !in.Now.Before(deadline) {
					plan.Nodes[si].Failed = true
					plan.TimedOut = append(plan.TimedOut, TimedOut{Pod: m.pod, Node: m.node, Group: m.group})
				} else {
					plan.requeue(deadline.Sub(in.Now))
				}
			}
			continue
		}
		plan.Release = append(plan.Release, rel)
	}
	return plan, nil
}

func groupName(g Group, j int) string {
	if g.Name != "" {
		return g.Name
	}
	return fmt.Sprintf("Group %d", j+1)
}

// groupSatisfiedOnNode reports whether group j is satisfied on node, the remaining Wait (if that is the
// only thing missing) and a human readable reason when not satisfied.
func groupSatisfiedOnNode(g Group, j int, node *corev1.Node, pods []*corev1.Pod, daemonSets []*appsv1.DaemonSet,
	now time.Time, ignoreTaintKeys []string) (bool, time.Duration, string) {
	name := groupName(g, j)
	for _, ds := range daemonSets {
		if !DaemonSetExpectedOnNode(ds, node, ignoreTaintKeys...) {
			continue
		}
		found := false
		for _, p := range pods {
			if OwnedBy(p, string(ds.UID)) {
				found = true
				break
			}
		}
		if !found {
			return false, 0, fmt.Sprintf("waiting for %s: DaemonSet %s has no pod on node yet", name, ds.Name)
		}
	}
	if len(pods) == 0 {
		if g.Policy == PolicyRequireOnNode {
			return false, 0, fmt.Sprintf("waiting for %s: no pods on node (policy RequireOnNode)", name)
		}
		return true, 0, ""
	}
	var maxRemaining time.Duration
	for _, p := range pods {
		if !IsPodReady(p) {
			return false, 0, fmt.Sprintf("waiting for %s: pod %s not Ready", name, p.Name)
		}
		if rem := ReadyRemaining(p, g.Wait, now); rem > maxRemaining {
			maxRemaining = rem
		}
	}
	if maxRemaining > 0 {
		return false, maxRemaining, fmt.Sprintf("waiting for %s: wait (%s remaining)", name, maxRemaining.Round(time.Second))
	}
	return true, 0, ""
}

func (p *Plan) requeue(d time.Duration) {
	if d <= 0 {
		return
	}
	if p.RequeueAfter == 0 || d < p.RequeueAfter {
		p.RequeueAfter = d
	}
}
