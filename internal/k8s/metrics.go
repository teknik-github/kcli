package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AddPodMetrics fills the CPU/Mem fields of pods from metrics-server, matching
// by namespace/name. It is best-effort: if metrics are unavailable the fields
// keep their "-" placeholder and no error is returned.
func (c *Client) AddPodMetrics(ctx context.Context, pods []Pod) {
	if c.metrics == nil || len(pods) == 0 {
		return
	}
	// A single namespace-scoped list is enough when all pods share it; use the
	// all-namespaces endpoint otherwise.
	ns := pods[0].Namespace
	for _, p := range pods {
		if p.Namespace != ns {
			ns = "" // mixed namespaces -> list across all
			break
		}
	}

	list, err := c.metrics.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	type usage struct{ cpu, mem string }
	byKey := make(map[string]usage, len(list.Items))
	for i := range list.Items {
		pm := &list.Items[i]
		var cpu, mem resource.Quantity
		for _, cont := range pm.Containers {
			cpu.Add(cont.Usage[corev1.ResourceCPU])
			mem.Add(cont.Usage[corev1.ResourceMemory])
		}
		byKey[pm.Namespace+"/"+pm.Name] = usage{formatCPU(&cpu), formatMem(&mem)}
	}

	for i := range pods {
		if u, ok := byKey[pods[i].Namespace+"/"+pods[i].Name]; ok {
			pods[i].CPU = u.cpu
			pods[i].Mem = u.mem
		}
	}
}

// AddNodeMetrics fills the CPU/Mem fields of nodes from metrics-server,
// including percent-of-capacity. Best-effort like AddPodMetrics.
func (c *Client) AddNodeMetrics(ctx context.Context, nodes []Node) {
	if c.metrics == nil || len(nodes) == 0 {
		return
	}

	list, err := c.metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	type usage struct{ cpuMilli, memBytes int64 }
	byName := make(map[string]usage, len(list.Items))
	for i := range list.Items {
		nm := &list.Items[i]
		cpu := nm.Usage[corev1.ResourceCPU]
		mem := nm.Usage[corev1.ResourceMemory]
		byName[nm.Name] = usage{cpu.MilliValue(), mem.Value()}
	}

	for i := range nodes {
		u, ok := byName[nodes[i].Name]
		if !ok {
			continue
		}
		nodes[i].CPU = fmt.Sprintf("%dm%s", u.cpuMilli, percent(u.cpuMilli, nodes[i].cpuCapMilli))
		nodes[i].Mem = fmt.Sprintf("%dMi%s", u.memBytes/(1024*1024), percent(u.memBytes, nodes[i].memCapBytes))
	}
}

// PodUsage returns a single pod's current CPU (millicores) and memory (bytes)
// usage, summed across containers, for live graphing.
func (c *Client) PodUsage(ctx context.Context, namespace, name string) (cpuMilli, memBytes int64, err error) {
	if c.metrics == nil {
		return 0, 0, fmt.Errorf("metrics-server unavailable")
	}
	pm, err := c.metrics.MetricsV1beta1().PodMetricses(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, 0, err
	}
	var cpu, mem resource.Quantity
	for _, cont := range pm.Containers {
		cpu.Add(cont.Usage[corev1.ResourceCPU])
		mem.Add(cont.Usage[corev1.ResourceMemory])
	}
	return cpu.MilliValue(), mem.Value(), nil
}

// NodeUsage returns a node's current CPU/memory usage plus its capacity, for
// live graphing with percent-of-capacity.
func (c *Client) NodeUsage(ctx context.Context, name string) (cpuMilli, memBytes, cpuCapMilli, memCapBytes int64, err error) {
	if c.metrics == nil {
		return 0, 0, 0, 0, fmt.Errorf("metrics-server unavailable")
	}
	nm, err := c.metrics.MetricsV1beta1().NodeMetricses().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, 0, 0, 0, err
	}
	node, err := c.clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, 0, 0, 0, err
	}
	cpu := nm.Usage[corev1.ResourceCPU]
	mem := nm.Usage[corev1.ResourceMemory]
	return cpu.MilliValue(), mem.Value(),
		node.Status.Capacity.Cpu().MilliValue(), node.Status.Capacity.Memory().Value(), nil
}

// formatCPU renders a CPU quantity as whole millicores, e.g. "12m".
func formatCPU(q *resource.Quantity) string {
	return fmt.Sprintf("%dm", q.MilliValue())
}

// formatMem renders a memory quantity in mebibytes, e.g. "34Mi".
func formatMem(q *resource.Quantity) string {
	return fmt.Sprintf("%dMi", q.Value()/(1024*1024))
}

// percent returns a " (NN%)" suffix, or "" when capacity is unknown.
func percent(used, capacity int64) string {
	if capacity <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%d%%)", used*100/capacity)
}
