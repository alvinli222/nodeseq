package nodeseq

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTargetNode(t *testing.T) {
	tests := []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{"bound", corev1.Pod{Spec: corev1.PodSpec{NodeName: "n1"}}, "n1"},
		{"daemonset pinned", corev1.Pod{Spec: corev1.PodSpec{Affinity: pinnedAffinity("n2")}}, "n2"},
		{"no affinity", corev1.Pod{}, ""},
		{"label affinity only", corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}},
			}}},
		}}}}, ""},
		{"terms disagree", corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}}},
				{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"b"}}}},
			}},
		}}}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TargetNode(&tt.pod); got != tt.want {
				t.Fatalf("TargetNode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGates(t *testing.T) {
	p := &corev1.Pod{Spec: corev1.PodSpec{SchedulingGates: []corev1.PodSchedulingGate{{Name: "a"}, {Name: gate}}}}
	if !HasGate(p, gate) || HasGate(p, "missing") {
		t.Fatal("HasGate wrong")
	}
	if g := WithoutGate(p, gate); len(g) != 1 || g[0].Name != "a" {
		t.Fatalf("WithoutGate = %+v", g)
	}
	p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: gate}}
	if WithoutGate(p, gate) != nil {
		t.Fatal("expected nil when no gates remain")
	}
}

func TestReadyRemaining(t *testing.T) {
	p := plainPod("p", nil, readyAt(t0))
	if !IsPodReady(&p) || ReadyRemaining(&p, 30*time.Second, t0.Add(10*time.Second)) != 20*time.Second {
		t.Fatal("unexpected ReadyRemaining")
	}
	if ReadyRemaining(&p, 0, t0) != 0 || ReadyRemaining(&p, time.Second, t0.Add(time.Minute)) != 0 {
		t.Fatal("expected 0 remaining")
	}
}

func TestIsActive(t *testing.T) {
	p := plainPod("p", nil)
	if !IsActive(&p) {
		t.Fatal("pending pod is active")
	}
	p.Status.Phase = corev1.PodSucceeded
	if IsActive(&p) {
		t.Fatal("succeeded pod is not active")
	}
	p.Status.Phase = corev1.PodRunning
	p.DeletionTimestamp = &metav1.Time{Time: t0}
	if IsActive(&p) {
		t.Fatal("terminating pod is not active")
	}
}

func TestDaemonSetShouldRunOnNode(t *testing.T) {
	ds := &appsv1.DaemonSet{Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{NodeSelector: map[string]string{"pool": "gpu"}},
	}}}
	gpu := mkNode("gpu", map[string]string{"pool": "gpu"})
	cpu := mkNode("cpu", map[string]string{"pool": "cpu"})
	if !DaemonSetShouldRunOnNode(ds, &gpu) || DaemonSetShouldRunOnNode(ds, &cpu) {
		t.Fatal("nodeSelector not honored")
	}
	tainted := gpu.DeepCopy()
	tainted.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "x", Effect: corev1.TaintEffectNoSchedule}}
	if DaemonSetShouldRunOnNode(ds, tainted) {
		t.Fatal("untolerated NoSchedule taint should block")
	}
	tainted.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule}}
	if !DaemonSetShouldRunOnNode(ds, tainted) {
		t.Fatal("DaemonSet default tolerations should apply")
	}
	tainted.Spec.Taints = []corev1.Taint{{Key: "x", Effect: corev1.TaintEffectPreferNoSchedule}}
	if !DaemonSetShouldRunOnNode(ds, tainted) {
		t.Fatal("PreferNoSchedule should not block")
	}
}
