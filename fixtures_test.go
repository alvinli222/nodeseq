package nodeseq

import (
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

const (
	ns   = "default"
	gate = "example.com/gate"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func boolPtr(b bool) *bool { return &b }

func mkNode(name string, lbls map[string]string, taints ...corev1.Taint) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls}, Spec: corev1.NodeSpec{Taints: taints}}
}

func mkDS(name string, lbls map[string]string) appsv1.DaemonSet {
	return appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-" + name)},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: lbls},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbls}},
		},
	}
}

type opt func(*corev1.Pod)

func gated() opt {
	return func(p *corev1.Pod) { p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: gate}} }
}

func readyAt(ts time.Time) opt {
	return func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(ts)}}
	}
}

func notReady() opt {
	return func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	}
}

func pinnedAffinity(node string) *corev1.Affinity {
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}},
		}}},
	}}
}

// dsPod builds a pod as the DaemonSet controller would: owned by ds and pinned to node.
func dsPod(name string, ds appsv1.DaemonSet, node string, opts ...opt) corev1.Pod {
	lbls := map[string]string{}
	for k, v := range ds.Spec.Template.Labels {
		lbls[k] = v
	}
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, Labels: lbls, CreationTimestamp: metav1.NewTime(t0),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: boolPtr(true)}},
		},
		Spec:   corev1.PodSpec{Affinity: pinnedAffinity(node)},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	for _, o := range opts {
		o(&p)
	}
	return p
}

func plainPod(name string, lbls map[string]string, opts ...opt) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbls, CreationTimestamp: metav1.NewTime(t0)},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	for _, o := range opts {
		o(&p)
	}
	return p
}

func grp(name string, lbls map[string]string) Group {
	return Group{Name: name, Selector: labels.SelectorFromSet(lbls)}
}

var (
	csiLabels = map[string]string{"app": "csi"}
	gpuLabels = map[string]string{"app": "gpu"}
	webLabels = map[string]string{"app": "web"}
)
