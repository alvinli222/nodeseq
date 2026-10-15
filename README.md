# nodeseq

**Node-level pod startup ordering for Kubernetes, as a pure Go library.**

`nodeseq` answers one question for every node: *which pods may start now?* You give it ordered groups of pods (label selectors), plus the nodes, pods and DaemonSets you already have in an informer cache. It returns, for each node, how many groups are satisfied, and which scheduling-gated pods can be released.

It makes no API calls, so it can be embedded in any controller: a namespaced add-on, a control-plane component, or a node agent.

```text
On EACH node independently:

group 0: [network agent, storage agent] --Ready--> group 1: [metrics agent] --Ready--> group 2: [node service]
```

## Why

Scheduling gates stop a pod from being scheduled, but Kubernetes has no built-in way to say "start this DaemonSet pod on node N only after that DaemonSet pod on node N is Ready". DaemonSet pods are **pinned** to their node before scheduling (required node affinity on `metadata.name`), so a controller can release each gated pod as soon as its dependencies are Ready **on the same node**. A slow node never holds up the others.

## Usage

```go
import (
	"github.com/alvinli222/nodeseq"
	"k8s.io/apimachinery/pkg/labels"
)

plan, err := nodeseq.Compute(nodeseq.Input{
	Groups: []nodeseq.Group{
		{Name: "foundation", Selector: labels.SelectorFromSet(labels.Set{"tier": "foundation"})},
		{Name: "metrics", Selector: labels.SelectorFromSet(labels.Set{"tier": "metrics"}), Wait: 10 * time.Second},
		{Name: "service", Selector: labels.SelectorFromSet(labels.Set{"tier": "service"}), Policy: nodeseq.PolicyRequireOnNode},
	},
	Namespace:  "kube-system",
	Gate:       "example.com/startup-gate",
	Nodes:      nodes,       // []corev1.Node from your cache
	Pods:       pods,        // []corev1.Pod
	DaemonSets: daemonSets,  // []appsv1.DaemonSet
	Now:        time.Now(),
})
for _, r := range plan.Release {
	// remove the gate from r.Pod (r.Reason explains why; r.Node == "" means unpinned)
}
// plan.Nodes[i].Satisfied: groups satisfied per node; plan.RequeueAfter: next time-based re-check
```

## Semantics

| Concept | Behaviour |
|---|---|
| **Group satisfied on a node** | Every active pod of the group targeting the node is `Ready` for at least `Wait`, **and** every DaemonSet in the group that is expected on the node already has a pod there. |
| **Expected on node** | Emulates the DaemonSet controller: template nodeSelector/affinity and taints vs. template + default DaemonSet tolerations. Transient lifecycle taints (`not-ready`, `unreachable`, `network-unavailable`, cloud `uninitialized`) and `IgnoreTaintKeys` are ignored, so a booting node is not mistaken for "not expected". |
| **No pods, nothing expected** | `PolicySkipIfNotScheduled` (default) counts the group as satisfied; `PolicyRequireOnNode` does not. |
| **Pinned pod** (DaemonSet pod, `spec.nodeName`) in group *k* | Released (`ReasonSatisfied`) when at least *k* groups are satisfied on its node. |
| **Unpinned pod** (Deployment, Job, ...) | Returned as `ReasonUnpinned`; the caller decides how to steer it (node label + affinity, taint + tolerations, ...). |
| `GateTimeout` | Pinned pods gated longer than this are listed in `Plan.TimedOut` and their node is marked `Failed`. **They are not released**, so the caller chooses fail-open or fail-closed. |
| `Bypass` | Optional predicate; matching gated pods are released immediately (`ReasonBypass`). |
| `NodeSelector` | Limits participating nodes. Pinned pods on other nodes are released (`ReasonNotParticipating`). |

Helpers are exported too: `TargetNode`, `HasGate`, `WithoutGate`, `IsPodReady`, `ReadyRemaining`, `IsActive`, `OwnedBy`, `DaemonSetShouldRunOnNode`, `DaemonSetExpectedOnNode`, `DaemonSetsForSelector`, `GroupIndex`.

## Limits

- Ordering applies to **scheduling** only. After a node reboot, the kubelet restarts bound pods without ordering; runtime ordering needs a node-local component (for example an init container waiting on a node-local signal).
- Requires Kubernetes 1.27+ (scheduling gates).

## Used by

- [podsequencing](https://github.com/alvinli222/podsequencing): `PodSequence` CRD controller (cluster and node scope, webhook gate injection, node label/taint eligibility).

## License

Apache 2.0
