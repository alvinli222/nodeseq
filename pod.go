package nodeseq

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// TargetNode returns the node a pod is bound or pinned to before scheduling. Pods created by the
// DaemonSet controller carry a required node affinity on metadata.name for exactly one node, so a
// scheduling-gated DaemonSet pod already knows its node. Returns "" when the node cannot be determined.
func TargetNode(pod *corev1.Pod) string {
	if pod.Spec.NodeName != "" {
		return pod.Spec.NodeName
	}
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil ||
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}
	terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	node := ""
	for _, term := range terms {
		termNode := ""
		for _, f := range term.MatchFields {
			if f.Key == "metadata.name" && f.Operator == corev1.NodeSelectorOpIn && len(f.Values) == 1 {
				termNode = f.Values[0]
			}
		}
		if termNode == "" || (node != "" && node != termNode) {
			return ""
		}
		node = termNode
	}
	return node
}

// IsActive reports whether the pod is neither terminating nor finished.
func IsActive(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp == nil &&
		pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// HasGate reports whether the pod carries the named scheduling gate.
func HasGate(pod *corev1.Pod, gate string) bool {
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name == gate {
			return true
		}
	}
	return false
}

// WithoutGate returns the pod's scheduling gates minus the named gate (nil if none remain).
func WithoutGate(pod *corev1.Pod, gate string) []corev1.PodSchedulingGate {
	var gates []corev1.PodSchedulingGate
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name != gate {
			gates = append(gates, g)
		}
	}
	return gates
}

// ReadyCondition returns the pod's Ready condition, if any.
func ReadyCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodReady {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// IsPodReady reports whether the pod's Ready condition is True.
func IsPodReady(pod *corev1.Pod) bool {
	c := ReadyCondition(pod)
	return c != nil && c.Status == corev1.ConditionTrue
}

// ReadyRemaining returns how much longer a Ready pod must stay Ready to satisfy wait (0 when satisfied).
// It is computed from the Ready condition's lastTransitionTime, so no state needs to be stored.
func ReadyRemaining(pod *corev1.Pod, wait time.Duration, now time.Time) time.Duration {
	if wait <= 0 {
		return 0
	}
	c := ReadyCondition(pod)
	if c == nil || c.LastTransitionTime.IsZero() {
		return 0
	}
	if remaining := c.LastTransitionTime.Add(wait).Sub(now); remaining > 0 {
		return remaining
	}
	return 0
}

// OwnedBy reports whether the pod's controller owner has the given UID.
func OwnedBy(pod *corev1.Pod, uid string) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller && string(ref.UID) == uid {
			return true
		}
	}
	return false
}
