package nodeseq

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func compute(t *testing.T, in Input) *Plan {
	t.Helper()
	if in.Gate == "" {
		in.Gate = gate
	}
	if in.Namespace == "" {
		in.Namespace = ns
	}
	if in.Now.IsZero() {
		in.Now = t0.Add(time.Minute)
	}
	p, err := Compute(in)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return p
}

func node(t *testing.T, p *Plan, name string) NodeState {
	t.Helper()
	s, ok := p.Node(name)
	if !ok {
		t.Fatalf("node %s not in plan", name)
	}
	return s
}

func released(p *Plan) map[string]Release {
	m := map[string]Release{}
	for _, r := range p.Release {
		m[r.Pod.Name] = r
	}
	return m
}

func TestPerNodeIndependence(t *testing.T) {
	csi, gpu := mkDS("csi", csiLabels), mkDS("gpu", gpuLabels)
	p := compute(t, Input{
		Groups:     []Group{grp("csi", csiLabels), grp("gpu", gpuLabels)},
		Nodes:      []corev1.Node{mkNode("n1", nil), mkNode("n2", nil)},
		DaemonSets: []appsv1.DaemonSet{csi, gpu},
		Pods: []corev1.Pod{
			dsPod("csi-1", csi, "n1", readyAt(t0)),
			dsPod("csi-2", csi, "n2", notReady()),
			dsPod("gpu-1", gpu, "n1", gated()),
			dsPod("gpu-2", gpu, "n2", gated()),
		},
	})
	if node(t, p, "n1").Satisfied != 1 || node(t, p, "n2").Satisfied != 0 {
		t.Fatalf("unexpected satisfied counts %+v", p.Nodes)
	}
	r := released(p)
	if r["gpu-1"].Reason != ReasonSatisfied || r["gpu-1"].Node != "n1" {
		t.Fatalf("gpu-1 should be released on n1, got %+v", r)
	}
	if _, ok := r["gpu-2"]; ok || p.GatedPods != 1 {
		t.Fatal("gpu-2 must stay gated")
	}
}

func TestExpectedDaemonSetPodMissing(t *testing.T) {
	csi, gpu := mkDS("csi", csiLabels), mkDS("gpu", gpuLabels)
	p := compute(t, Input{
		Groups:     []Group{grp("csi", csiLabels), grp("gpu", gpuLabels)},
		Nodes:      []corev1.Node{mkNode("new", nil)},
		DaemonSets: []appsv1.DaemonSet{csi, gpu},
		Pods:       []corev1.Pod{dsPod("gpu-new", gpu, "new", gated())},
	})
	if node(t, p, "new").Satisfied != 0 || len(p.Release) != 0 {
		t.Fatal("a node without the expected DaemonSet pod must not be satisfied")
	}
}

func TestDaemonSetNotExpectedIsSkipped(t *testing.T) {
	csi, gpu := mkDS("csi", csiLabels), mkDS("gpu", gpuLabels)
	csi.Spec.Template.Spec.NodeSelector = map[string]string{"storage": "true"}
	p := compute(t, Input{
		Groups:     []Group{grp("csi", csiLabels), grp("gpu", gpuLabels)},
		Nodes:      []corev1.Node{mkNode("n1", nil)},
		DaemonSets: []appsv1.DaemonSet{csi, gpu},
		Pods:       []corev1.Pod{dsPod("gpu-1", gpu, "n1", gated())},
	})
	if released(p)["gpu-1"].Reason != ReasonSatisfied {
		t.Fatal("csi is not expected on n1, so gpu-1 should be released")
	}
}

func TestRequireOnNode(t *testing.T) {
	gpu := mkDS("gpu", gpuLabels)
	g0 := grp("csi", csiLabels)
	g0.Policy = PolicyRequireOnNode
	p := compute(t, Input{
		Groups: []Group{g0, grp("gpu", gpuLabels)}, Nodes: []corev1.Node{mkNode("n1", nil)},
		DaemonSets: []appsv1.DaemonSet{gpu}, Pods: []corev1.Pod{dsPod("gpu-1", gpu, "n1", gated())},
	})
	if len(p.Release) != 0 {
		t.Fatal("RequireOnNode must block when the group has no pods on the node")
	}
}

func TestWait(t *testing.T) {
	csi, gpu := mkDS("csi", csiLabels), mkDS("gpu", gpuLabels)
	g0 := grp("csi", csiLabels)
	g0.Wait = 30 * time.Second
	in := Input{
		Groups: []Group{g0, grp("gpu", gpuLabels)}, Nodes: []corev1.Node{mkNode("n1", nil)},
		DaemonSets: []appsv1.DaemonSet{csi, gpu},
		Pods:       []corev1.Pod{dsPod("csi-1", csi, "n1", readyAt(t0)), dsPod("gpu-1", gpu, "n1", gated())},
		Now:        t0.Add(10 * time.Second),
	}
	p := compute(t, in)
	if len(p.Release) != 0 || p.RequeueAfter != 20*time.Second {
		t.Fatalf("should wait 20s more, RequeueAfter=%v", p.RequeueAfter)
	}
	in.Now = t0.Add(31 * time.Second)
	if p := compute(t, in); len(p.Release) != 1 {
		t.Fatal("should release after Wait")
	}
}

func TestUnpinnedPodsAreReturnedToCaller(t *testing.T) {
	csi := mkDS("csi", csiLabels)
	p := compute(t, Input{
		Groups: []Group{grp("csi", csiLabels), grp("web", webLabels)}, Nodes: []corev1.Node{mkNode("n1", nil)},
		DaemonSets: []appsv1.DaemonSet{csi}, Pods: []corev1.Pod{plainPod("web-1", webLabels, gated())},
	})
	r := released(p)["web-1"]
	if r.Reason != ReasonUnpinned || r.Group != 1 || r.Node != "" {
		t.Fatalf("unexpected release %+v", r)
	}
}

func TestGateTimeoutKeepsPodGated(t *testing.T) {
	csi, gpu := mkDS("csi", csiLabels), mkDS("gpu", gpuLabels)
	in := Input{
		Groups: []Group{grp("csi", csiLabels), grp("gpu", gpuLabels)}, Nodes: []corev1.Node{mkNode("n1", nil)},
		DaemonSets:  []appsv1.DaemonSet{csi, gpu},
		Pods:        []corev1.Pod{dsPod("csi-1", csi, "n1", notReady()), dsPod("gpu-1", gpu, "n1", gated())},
		GateTimeout: 5 * time.Minute, Now: t0.Add(time.Minute),
	}
	if p := compute(t, in); node(t, p, "n1").Failed || p.RequeueAfter != 4*time.Minute {
		t.Fatalf("not timed out yet; RequeueAfter=%v", p.RequeueAfter)
	}
	in.Now = t0.Add(6 * time.Minute)
	p := compute(t, in)
	if !node(t, p, "n1").Failed || len(p.TimedOut) != 1 || len(p.Release) != 0 || p.GatedPods != 1 {
		t.Fatal("timed-out pod must be reported and stay gated")
	}
}

func TestBypassAndNotParticipating(t *testing.T) {
	csi, gpu := mkDS("csi", csiLabels), mkDS("gpu", gpuLabels)
	bypassed := dsPod("gpu-bypass", gpu, "gpu-node", gated())
	bypassed.Annotations = map[string]string{"bypass": "true"}
	p := compute(t, Input{
		Groups:       []Group{grp("csi", csiLabels), grp("gpu", gpuLabels)},
		NodeSelector: labels.SelectorFromSet(map[string]string{"pool": "gpu"}),
		Nodes:        []corev1.Node{mkNode("gpu-node", map[string]string{"pool": "gpu"}), mkNode("cpu-node", nil)},
		DaemonSets:   []appsv1.DaemonSet{csi, gpu},
		Pods:         []corev1.Pod{dsPod("csi-1", csi, "gpu-node", notReady()), bypassed, dsPod("gpu-cpu", gpu, "cpu-node", gated())},
		Bypass:       func(p *corev1.Pod) bool { return p.Annotations["bypass"] == "true" },
	})
	r := released(p)
	if r["gpu-bypass"].Reason != ReasonBypass || r["gpu-cpu"].Reason != ReasonNotParticipating {
		t.Fatalf("unexpected releases %+v", r)
	}
}

func TestIgnoredAndTransientTaints(t *testing.T) {
	csi, gpu := mkDS("csi", csiLabels), mkDS("gpu", gpuLabels)
	own := "example.com/owned"
	n := mkNode("n1", nil,
		corev1.Taint{Key: own, Value: "0", Effect: corev1.TaintEffectNoSchedule},
		corev1.Taint{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoSchedule})
	in := Input{
		Groups: []Group{grp("csi", csiLabels), grp("gpu", gpuLabels)}, Nodes: []corev1.Node{n},
		DaemonSets: []appsv1.DaemonSet{csi, gpu}, Pods: []corev1.Pod{dsPod("gpu-1", gpu, "n1", gated())},
		IgnoreTaintKeys: []string{own},
	}
	if p := compute(t, in); node(t, p, "n1").Satisfied != 0 || len(p.Release) != 0 {
		t.Fatal("csi is only kept off by ignored/transient taints and must hold its group unsatisfied")
	}
	in.Nodes = []corev1.Node{mkNode("n1", nil, corev1.Taint{Key: "dedicated", Effect: corev1.TaintEffectNoSchedule})}
	if p := compute(t, in); node(t, p, "n1").Satisfied != 1 {
		t.Fatal("a real untolerated taint means csi is not expected on the node")
	}
}

func TestNamespaceFiltering(t *testing.T) {
	gpu := mkDS("gpu", gpuLabels)
	other := dsPod("gpu-other", gpu, "n1", gated())
	other.Namespace = "other"
	p := compute(t, Input{
		Groups: []Group{grp("csi", csiLabels), grp("gpu", gpuLabels)}, Nodes: []corev1.Node{mkNode("n1", nil)},
		Pods: []corev1.Pod{other},
	})
	if len(p.Release) != 0 || p.GatedPods != 0 {
		t.Fatal("pods outside the namespace must be ignored")
	}
}

func TestComputeValidation(t *testing.T) {
	if _, err := Compute(Input{Groups: []Group{{Name: "x"}}}); err == nil {
		t.Fatal("expected error for missing selector")
	}
	if _, err := Compute(Input{Groups: []Group{{Selector: labels.Everything(), Policy: "Bogus"}}}); err == nil {
		t.Fatal("expected error for unknown policy")
	}
}
