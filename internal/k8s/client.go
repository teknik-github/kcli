// Package k8s wraps the Kubernetes client-go APIs that kcli needs.
package k8s

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/yaml"
)

// Client is a thin wrapper over a typed clientset plus the resolved context
// metadata that the UI wants to display.
type Client struct {
	clientset  *kubernetes.Clientset
	metrics    *metricsv.Clientset // nil if metrics-server is unavailable
	restCfg    *rest.Config        // kept for streaming subresources (exec)
	Context    string              // current kubeconfig context name
	kubeconfig string              // resolved kubeconfig path, for switching context

	mapper    meta.RESTMapper   // lazily built discovery REST mapper (dynamic/apply)
	dynClient dynamic.Interface // lazily built dynamic client (generic/CRD views)

	// Shared informer cache (informer.go). Listers read from it and fall back to
	// a live List when a resource can't sync; onChange fires the UI's live refresh.
	infMu      sync.Mutex
	infFactory informers.SharedInformerFactory
	infStop    chan struct{}
	infStarted map[schema.GroupVersionResource]bool
	onChange   func()
}

// NewClient builds a Client from a kubeconfig path. An empty path falls back to
// the standard resolution: $KUBECONFIG, then ~/.kube/config, then in-cluster.
func NewClient(kubeconfig string) (*Client, error) {
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}
	c := &Client{kubeconfig: kubeconfig}
	if err := c.connect(""); err != nil {
		return nil, err
	}
	return c, nil
}

// loadingRules returns the kubeconfig loading rules for this client's path.
func (c *Client) loadingRules() *clientcmd.ClientConfigLoadingRules {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if c.kubeconfig != "" {
		rules.ExplicitPath = c.kubeconfig
	}
	return rules
}

// connect (re)builds the clientset/metrics/restCfg for the given context and
// stores them on the receiver. An empty contextName uses the kubeconfig's
// current-context; on failure it falls back to in-cluster config.
func (c *Client) connect(contextName string) error {
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	cfgLoader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(c.loadingRules(), overrides)

	restCfg, err := cfgLoader.ClientConfig()
	if err != nil {
		// Fall back to in-cluster config (running as a pod).
		restCfg, err = rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("no usable kubeconfig or in-cluster config: %w", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build clientset: %w", err)
	}

	ctxName := contextName
	if ctxName == "" {
		if raw, err := cfgLoader.RawConfig(); err == nil {
			ctxName = raw.CurrentContext
		}
	}

	// Metrics are best-effort: a cluster without metrics-server still works,
	// the CPU/MEM columns just render "-".
	metricsClient, _ := metricsv.NewForConfig(restCfg)

	c.clientset = clientset
	c.metrics = metricsClient
	c.restCfg = restCfg
	c.Context = ctxName
	return nil
}

// Contexts lists the context names defined in the kubeconfig, sorted.
func (c *Client) Contexts() ([]string, error) {
	raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		c.loadingRules(), &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(raw.Contexts))
	for name := range raw.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// WithContext builds a fresh Client pointed at a different kubeconfig context,
// leaving the receiver untouched. Building is local (no network); the first
// request against the new context is what actually reaches the cluster.
func (c *Client) WithContext(contextName string) (*Client, error) {
	nc := &Client{kubeconfig: c.kubeconfig}
	if err := nc.connect(contextName); err != nil {
		return nil, err
	}
	return nc, nil
}

// Pod is a display-oriented snapshot of a pod, flattened for a TUI table.
type Pod struct {
	Name      string
	Namespace string
	Ready     string // e.g. "2/2"
	Status    string // Running, Pending, CrashLoopBackOff, ...
	Restarts  int32
	CPU       string // usage in millicores, e.g. "12m" ("-" if no metrics)
	Mem       string // usage, e.g. "34Mi" ("-" if no metrics)
	Age       string // humanized, e.g. "3d"
	Node      string
}

// Namespaces returns namespace names sorted alphabetically.
func (c *Client) Namespaces(ctx context.Context) ([]string, error) {
	list, err := c.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, ns := range list.Items {
		names = append(names, ns.Name)
	}
	sort.Strings(names)
	return names, nil
}

// Pods lists pods in a namespace. An empty namespace lists across all namespaces.
// Reads from the informer cache when available, else a live List.
func (c *Client) Pods(ctx context.Context, namespace string) ([]Pod, error) {
	var pods []Pod
	if objs, ok, err := c.cachedObjects(ctx, gvrPods, namespace); ok && err == nil {
		pods = make([]Pod, 0, len(objs))
		for _, o := range objs {
			if p, ok := o.(*corev1.Pod); ok {
				pods = append(pods, toPod(p))
			}
		}
	} else {
		list, lerr := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		pods = make([]Pod, 0, len(list.Items))
		for i := range list.Items {
			pods = append(pods, toPod(&list.Items[i]))
		}
	}
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Name < pods[j].Name
	})
	return pods, nil
}

// Deployment is a display-oriented snapshot of a deployment.
type Deployment struct {
	Name      string
	Namespace string
	Ready     string // e.g. "3/3" (ready/desired replicas)
	Desired   int32  // spec.replicas, used to prefill the scale dialog
	UpToDate  int32
	Available int32
	Age       string
}

// Service is a display-oriented snapshot of a service.
type Service struct {
	Name       string
	Namespace  string
	Type       string // ClusterIP, NodePort, LoadBalancer, ExternalName
	ClusterIP  string
	ExternalIP string
	Ports      string // e.g. "80/TCP,443/TCP"
	Age        string
}

// Node is a display-oriented snapshot of a cluster node.
type Node struct {
	Name    string
	Status  string // Ready, NotReady, ...
	Roles   string // e.g. "control-plane" or "<none>"
	Version string // kubelet version
	CPU     string // usage + percent of capacity, e.g. "237m (5%)"
	Mem     string // usage + percent of capacity, e.g. "7550Mi (58%)"
	Age     string

	cpuCapMilli int64 // capacity, for percent calculation
	memCapBytes int64
}

// ConfigMap is a display-oriented snapshot of a configmap.
type ConfigMap struct {
	Name      string
	Namespace string
	Data      int // number of data + binaryData keys
	Age       string
}

// Secret is a display-oriented snapshot of a secret. Values are never carried
// here — only metadata and the key count.
type Secret struct {
	Name      string
	Namespace string
	Type      string // e.g. "Opaque", "kubernetes.io/tls"
	Data      int    // number of keys
	Age       string
}

// toSecret flattens a corev1.Secret to metadata only — values are never carried.
func toSecret(s *corev1.Secret) Secret {
	return Secret{
		Name:      s.Name,
		Namespace: s.Namespace,
		Type:      string(s.Type),
		Data:      len(s.Data),
		Age:       humanAge(s.CreationTimestamp.Time),
	}
}

// Secrets lists secrets in a namespace ("" = all namespaces). Only metadata is
// returned; secret values are deliberately omitted.
func (c *Client) Secrets(ctx context.Context, namespace string) ([]Secret, error) {
	var out []Secret
	if objs, ok, err := c.cachedObjects(ctx, gvrSecrets, namespace); ok && err == nil {
		out = make([]Secret, 0, len(objs))
		for _, o := range objs {
			if s, ok := o.(*corev1.Secret); ok {
				out = append(out, toSecret(s))
			}
		}
	} else {
		list, lerr := c.clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		out = make([]Secret, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, toSecret(&list.Items[i]))
		}
	}
	sortByNsName(len(out),
		func(i int) (string, string) { return out[i].Namespace, out[i].Name },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// SecretData returns a secret's decoded values (client-go base64-decodes the
// Data map into raw bytes; we surface them as strings). Used by the explicit
// "reveal" action — normal display never carries values.
func (c *Client) SecretData(ctx context.Context, namespace, name string) (map[string]string, error) {
	s, err := c.clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(s.Data))
	for k, v := range s.Data {
		out[k] = string(v)
	}
	return out, nil
}

// ServiceForward resolves a Service to a Ready backing pod and rewrites the
// requested local:servicePort mappings to local:podPort (following targetPort,
// including named ports), so a port-forward against a Service transparently
// reaches the pod's real container port — like `kubectl port-forward service/…`.
// It errors on selector-less services (headless/ExternalName), which have no
// pods to forward to.
func (c *Client) ServiceForward(ctx context.Context, namespace, name string, ports []string) (string, []string, error) {
	svc, err := c.clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", nil, err
	}
	if len(svc.Spec.Selector) == 0 {
		return "", nil, fmt.Errorf("service %s has no selector (headless or ExternalName)", name)
	}
	sel := metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: svc.Spec.Selector})
	pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return "", nil, err
	}

	var target, firstRunning *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		if firstRunning == nil {
			firstRunning = p
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				target = p // prefer a Ready pod
				break
			}
		}
		if target != nil {
			break
		}
	}
	if target == nil {
		target = firstRunning // no Ready pod, but a Running one will do
	}
	if target == nil {
		return "", nil, fmt.Errorf("no running pod behind service %s", name)
	}

	// A Service port often differs from the pod's container port (via targetPort,
	// which may be a named port). Translate each requested local:servicePort to
	// local:podPort so forwarding to a Service behaves like kubectl.
	svcToPod := make(map[int]int, len(svc.Spec.Ports))
	for _, sp := range svc.Spec.Ports {
		svcToPod[int(sp.Port)] = targetPodPort(sp, target)
	}
	mapped := make([]string, len(ports))
	for i, pm := range ports {
		local, remote, found := strings.Cut(pm, ":")
		rp, err := strconv.Atoi(remote)
		if !found || err != nil {
			mapped[i] = pm
			continue
		}
		if pp, ok := svcToPod[rp]; ok && pp != rp {
			mapped[i] = local + ":" + strconv.Itoa(pp)
		} else {
			mapped[i] = pm
		}
	}
	return target.Name, mapped, nil
}

// targetPodPort resolves a Service port's targetPort (numeric, named, or unset)
// to the backing pod's container port number.
func targetPodPort(sp corev1.ServicePort, pod *corev1.Pod) int {
	tp := sp.TargetPort
	if tp.Type == intstr.Int {
		if tp.IntVal != 0 {
			return int(tp.IntVal)
		}
		return int(sp.Port) // unset targetPort defaults to the service port
	}
	for _, ct := range pod.Spec.Containers {
		for _, cp := range ct.Ports {
			if cp.Name == tp.StrVal {
				return int(cp.ContainerPort)
			}
		}
	}
	return int(sp.Port) // named but not found on the pod; fall back
}

// Ingress is a display-oriented snapshot of an ingress.
type Ingress struct {
	Name      string
	Namespace string
	Class     string
	Hosts     string
	Address   string
	Ports     string
	Age       string
}

// Job is a display-oriented snapshot of a batch job.
type Job struct {
	Name        string
	Namespace   string
	Completions string // e.g. "1/1"
	Duration    string
	Age         string
}

// StatefulSet is a display-oriented snapshot of a statefulset.
type StatefulSet struct {
	Name      string
	Namespace string
	Ready     string // "ready/desired"
	Desired   int32  // for the scale dialog prefill
	Age       string
}

// PVC is a display-oriented snapshot of a persistent volume claim.
type PVC struct {
	Name         string
	Namespace    string
	Status       string // Bound, Pending, Lost
	Volume       string
	Capacity     string
	AccessModes  string
	StorageClass string
	Age          string
}

// toStatefulSet flattens an appsv1.StatefulSet for display.
func toStatefulSet(s *appsv1.StatefulSet) StatefulSet {
	desired := int32(0)
	if s.Spec.Replicas != nil {
		desired = *s.Spec.Replicas
	}
	return StatefulSet{
		Name:      s.Name,
		Namespace: s.Namespace,
		Ready:     fmt.Sprintf("%d/%d", s.Status.ReadyReplicas, desired),
		Desired:   desired,
		Age:       humanAge(s.CreationTimestamp.Time),
	}
}

// StatefulSets lists statefulsets in a namespace ("" = all namespaces).
func (c *Client) StatefulSets(ctx context.Context, namespace string) ([]StatefulSet, error) {
	var out []StatefulSet
	if objs, ok, err := c.cachedObjects(ctx, gvrStatefulSets, namespace); ok && err == nil {
		out = make([]StatefulSet, 0, len(objs))
		for _, o := range objs {
			if s, ok := o.(*appsv1.StatefulSet); ok {
				out = append(out, toStatefulSet(s))
			}
		}
	} else {
		list, lerr := c.clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		out = make([]StatefulSet, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, toStatefulSet(&list.Items[i]))
		}
	}
	sortByNsName(len(out),
		func(i int) (string, string) { return out[i].Namespace, out[i].Name },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// PVCs lists persistent volume claims in a namespace ("" = all namespaces).
func (c *Client) PVCs(ctx context.Context, namespace string) ([]PVC, error) {
	var out []PVC
	if objs, ok, err := c.cachedObjects(ctx, gvrPVCs, namespace); ok && err == nil {
		out = make([]PVC, 0, len(objs))
		for _, o := range objs {
			if p, ok := o.(*corev1.PersistentVolumeClaim); ok {
				out = append(out, toPVC(p))
			}
		}
	} else {
		list, lerr := c.clientset.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		out = make([]PVC, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, toPVC(&list.Items[i]))
		}
	}
	sortByNsName(len(out),
		func(i int) (string, string) { return out[i].Namespace, out[i].Name },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// ScaleStatefulSet sets a statefulset's replica count via the scale subresource.
func (c *Client) ScaleStatefulSet(ctx context.Context, namespace, name string, replicas int32) error {
	sts := c.clientset.AppsV1().StatefulSets(namespace)
	scale, err := sts.GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	scale.Spec.Replicas = replicas
	_, err = sts.UpdateScale(ctx, name, scale, metav1.UpdateOptions{})
	return err
}

// Scale dispatches replica changes by kind ("deployment", "statefulset").
func (c *Client) Scale(ctx context.Context, kind, namespace, name string, replicas int32) error {
	switch kind {
	case "deployment":
		return c.ScaleDeployment(ctx, namespace, name, replicas)
	case "statefulset":
		return c.ScaleStatefulSet(ctx, namespace, name, replicas)
	default:
		return fmt.Errorf("cannot scale kind %q", kind)
	}
}

// Ingresses lists ingresses in a namespace ("" = all namespaces).
func (c *Client) Ingresses(ctx context.Context, namespace string) ([]Ingress, error) {
	var out []Ingress
	if objs, ok, err := c.cachedObjects(ctx, gvrIngresses, namespace); ok && err == nil {
		out = make([]Ingress, 0, len(objs))
		for _, o := range objs {
			if in, ok := o.(*networkingv1.Ingress); ok {
				out = append(out, toIngress(in))
			}
		}
	} else {
		list, lerr := c.clientset.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		out = make([]Ingress, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, toIngress(&list.Items[i]))
		}
	}
	sortByNsName(len(out),
		func(i int) (string, string) { return out[i].Namespace, out[i].Name },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// Jobs lists jobs in a namespace ("" = all namespaces).
func (c *Client) Jobs(ctx context.Context, namespace string) ([]Job, error) {
	var out []Job
	if objs, ok, err := c.cachedObjects(ctx, gvrJobs, namespace); ok && err == nil {
		out = make([]Job, 0, len(objs))
		for _, o := range objs {
			if j, ok := o.(*batchv1.Job); ok {
				out = append(out, toJob(j))
			}
		}
	} else {
		list, lerr := c.clientset.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		out = make([]Job, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, toJob(&list.Items[i]))
		}
	}
	sortByNsName(len(out),
		func(i int) (string, string) { return out[i].Namespace, out[i].Name },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// Nodes lists cluster nodes (cluster-scoped, no namespace).
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var out []Node
	if objs, ok, err := c.cachedObjects(ctx, gvrNodes, ""); ok && err == nil {
		out = make([]Node, 0, len(objs))
		for _, o := range objs {
			if n, ok := o.(*corev1.Node); ok {
				out = append(out, toNode(n))
			}
		}
	} else {
		list, lerr := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		out = make([]Node, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, toNode(&list.Items[i]))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// toConfigMap flattens a corev1.ConfigMap for display (key count only).
func toConfigMap(cm *corev1.ConfigMap) ConfigMap {
	return ConfigMap{
		Name:      cm.Name,
		Namespace: cm.Namespace,
		Data:      len(cm.Data) + len(cm.BinaryData),
		Age:       humanAge(cm.CreationTimestamp.Time),
	}
}

// ConfigMaps lists configmaps in a namespace ("" = all namespaces).
func (c *Client) ConfigMaps(ctx context.Context, namespace string) ([]ConfigMap, error) {
	var out []ConfigMap
	if objs, ok, err := c.cachedObjects(ctx, gvrConfigMaps, namespace); ok && err == nil {
		out = make([]ConfigMap, 0, len(objs))
		for _, o := range objs {
			if cm, ok := o.(*corev1.ConfigMap); ok {
				out = append(out, toConfigMap(cm))
			}
		}
	} else {
		list, lerr := c.clientset.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		out = make([]ConfigMap, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, toConfigMap(&list.Items[i]))
		}
	}
	sortByNsName(len(out),
		func(i int) (string, string) { return out[i].Namespace, out[i].Name },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// Deployments lists deployments in a namespace ("" = all namespaces).
func (c *Client) Deployments(ctx context.Context, namespace string) ([]Deployment, error) {
	var out []Deployment
	if objs, ok, err := c.cachedObjects(ctx, gvrDeployments, namespace); ok && err == nil {
		out = make([]Deployment, 0, len(objs))
		for _, o := range objs {
			if d, ok := o.(*appsv1.Deployment); ok {
				out = append(out, toDeployment(d))
			}
		}
	} else {
		list, lerr := c.clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		out = make([]Deployment, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, toDeployment(&list.Items[i]))
		}
	}
	sortByNsName(len(out),
		func(i int) (string, string) { return out[i].Namespace, out[i].Name },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// Services lists services in a namespace ("" = all namespaces).
func (c *Client) Services(ctx context.Context, namespace string) ([]Service, error) {
	var out []Service
	if objs, ok, err := c.cachedObjects(ctx, gvrServices, namespace); ok && err == nil {
		out = make([]Service, 0, len(objs))
		for _, o := range objs {
			if s, ok := o.(*corev1.Service); ok {
				out = append(out, toService(s))
			}
		}
	} else {
		list, lerr := c.clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		out = make([]Service, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, toService(&list.Items[i]))
		}
	}
	sortByNsName(len(out),
		func(i int) (string, string) { return out[i].Namespace, out[i].Name },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// DaemonSet is a display-oriented snapshot of a daemonset.
type DaemonSet struct {
	Name      string
	Namespace string
	Desired   int32
	Current   int32
	Ready     int32
	UpToDate  int32
	Available int32
	Age       string
}

// toDaemonSet flattens an appsv1.DaemonSet for display.
func toDaemonSet(d *appsv1.DaemonSet) DaemonSet {
	return DaemonSet{
		Name:      d.Name,
		Namespace: d.Namespace,
		Desired:   d.Status.DesiredNumberScheduled,
		Current:   d.Status.CurrentNumberScheduled,
		Ready:     d.Status.NumberReady,
		UpToDate:  d.Status.UpdatedNumberScheduled,
		Available: d.Status.NumberAvailable,
		Age:       humanAge(d.CreationTimestamp.Time),
	}
}

// DaemonSets lists daemonsets in a namespace ("" = all namespaces).
func (c *Client) DaemonSets(ctx context.Context, namespace string) ([]DaemonSet, error) {
	var out []DaemonSet
	if objs, ok, err := c.cachedObjects(ctx, gvrDaemonSets, namespace); ok && err == nil {
		out = make([]DaemonSet, 0, len(objs))
		for _, o := range objs {
			if d, ok := o.(*appsv1.DaemonSet); ok {
				out = append(out, toDaemonSet(d))
			}
		}
	} else {
		list, lerr := c.clientset.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		out = make([]DaemonSet, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, toDaemonSet(&list.Items[i]))
		}
	}
	sortByNsName(len(out),
		func(i int) (string, string) { return out[i].Namespace, out[i].Name },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// ReplicaSet is a display-oriented snapshot of a replicaset.
type ReplicaSet struct {
	Name      string
	Namespace string
	Desired   int32
	Current   int32
	Ready     int32
	Age       string
}

// toReplicaSet flattens an appsv1.ReplicaSet for display.
func toReplicaSet(r *appsv1.ReplicaSet) ReplicaSet {
	desired := int32(0)
	if r.Spec.Replicas != nil {
		desired = *r.Spec.Replicas
	}
	return ReplicaSet{
		Name:      r.Name,
		Namespace: r.Namespace,
		Desired:   desired,
		Current:   r.Status.Replicas,
		Ready:     r.Status.ReadyReplicas,
		Age:       humanAge(r.CreationTimestamp.Time),
	}
}

// ReplicaSets lists replicasets in a namespace ("" = all namespaces).
func (c *Client) ReplicaSets(ctx context.Context, namespace string) ([]ReplicaSet, error) {
	var out []ReplicaSet
	if objs, ok, err := c.cachedObjects(ctx, gvrReplicaSets, namespace); ok && err == nil {
		out = make([]ReplicaSet, 0, len(objs))
		for _, o := range objs {
			if r, ok := o.(*appsv1.ReplicaSet); ok {
				out = append(out, toReplicaSet(r))
			}
		}
	} else {
		list, lerr := c.clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		out = make([]ReplicaSet, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, toReplicaSet(&list.Items[i]))
		}
	}
	sortByNsName(len(out),
		func(i int) (string, string) { return out[i].Namespace, out[i].Name },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// CronJob is a display-oriented snapshot of a cronjob.
type CronJob struct {
	Name         string
	Namespace    string
	Schedule     string
	Suspend      bool
	Active       int
	LastSchedule string
	Age          string
}

// toCronJob flattens a batchv1.CronJob for display.
func toCronJob(cj *batchv1.CronJob) CronJob {
	suspend := false
	if cj.Spec.Suspend != nil {
		suspend = *cj.Spec.Suspend
	}
	last := "<none>"
	if cj.Status.LastScheduleTime != nil {
		last = humanAge(cj.Status.LastScheduleTime.Time)
	}
	return CronJob{
		Name:         cj.Name,
		Namespace:    cj.Namespace,
		Schedule:     cj.Spec.Schedule,
		Suspend:      suspend,
		Active:       len(cj.Status.Active),
		LastSchedule: last,
		Age:          humanAge(cj.CreationTimestamp.Time),
	}
}

// CronJobs lists cronjobs in a namespace ("" = all namespaces).
func (c *Client) CronJobs(ctx context.Context, namespace string) ([]CronJob, error) {
	var out []CronJob
	if objs, ok, err := c.cachedObjects(ctx, gvrCronJobs, namespace); ok && err == nil {
		out = make([]CronJob, 0, len(objs))
		for _, o := range objs {
			if cj, ok := o.(*batchv1.CronJob); ok {
				out = append(out, toCronJob(cj))
			}
		}
	} else {
		list, lerr := c.clientset.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		out = make([]CronJob, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, toCronJob(&list.Items[i]))
		}
	}
	sortByNsName(len(out),
		func(i int) (string, string) { return out[i].Namespace, out[i].Name },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// RolloutRestart triggers a rolling restart of a workload by stamping its pod
// template with the same annotation `kubectl rollout restart` uses, forcing the
// controller to recreate pods. kind is "deployment", "statefulset", or
// "daemonset".
func (c *Client) RolloutRestart(ctx context.Context, kind, namespace, name string) error {
	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().Format(time.RFC3339)))
	switch kind {
	case "deployment":
		_, err := c.clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
		return err
	case "statefulset":
		_, err := c.clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
		return err
	case "daemonset":
		_, err := c.clientset.AppsV1().DaemonSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
		return err
	default:
		return fmt.Errorf("cannot restart kind %q", kind)
	}
}

// CordonNode toggles a node's schedulability by patching spec.unschedulable.
func (c *Client) CordonNode(ctx context.Context, name string, cordon bool) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, cordon))
	_, err := c.clientset.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}

// DrainNode cordons a node then evicts its evictable pods via the Eviction API
// (which respects PodDisruptionBudgets). DaemonSet-managed and static/mirror
// pods are skipped, matching `kubectl drain` defaults. It returns how many pods
// were evicted; eviction errors for individual pods do not stop the drain, the
// first one is returned once every pod has been attempted.
func (c *Client) DrainNode(ctx context.Context, name string) (evicted int, err error) {
	if cerr := c.CordonNode(ctx, name, true); cerr != nil {
		return 0, fmt.Errorf("cordon: %w", cerr)
	}
	pods, lerr := c.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", name).String(),
	})
	if lerr != nil {
		return 0, lerr
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if skipDrain(p) {
			continue
		}
		ev := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
		}
		if e := c.clientset.PolicyV1().Evictions(p.Namespace).Evict(ctx, ev); e != nil {
			if err == nil {
				err = fmt.Errorf("evict %s/%s: %w", p.Namespace, p.Name, e)
			}
			continue
		}
		evicted++
	}
	return evicted, err
}

// skipDrain reports pods a drain must not evict: DaemonSet-managed pods (they
// would be recreated on the node immediately), static/mirror pods (not backed
// by the API), and already-terminal pods.
func skipDrain(p *corev1.Pod) bool {
	if _, ok := p.Annotations["kubernetes.io/config.mirror"]; ok {
		return true
	}
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return true
		}
	}
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// Event is a display-oriented snapshot of a cluster event. Name is the event
// object's own name (used for Describe), not shown as a column.
type Event struct {
	Name      string
	Namespace string
	Type      string // Normal, Warning
	Reason    string
	Object    string // involved object as Kind/Name
	Count     int32
	LastSeen  string
	Message   string
}

// toEvent flattens a corev1.Event for display.
func toEvent(e *corev1.Event) Event {
	obj := e.InvolvedObject.Name
	if e.InvolvedObject.Kind != "" {
		obj = e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name
	}
	count := e.Count
	if count == 0 {
		count = 1
	}
	return Event{
		Name:      e.Name,
		Namespace: e.Namespace,
		Type:      e.Type,
		Reason:    e.Reason,
		Object:    obj,
		Count:     count,
		LastSeen:  humanAge(eventTime(e)),
		Message:   strings.ReplaceAll(e.Message, "\n", " "),
	}
}

// Events lists events in a namespace ("" = all namespaces), newest first.
func (c *Client) Events(ctx context.Context, namespace string) ([]Event, error) {
	var items []*corev1.Event
	if objs, ok, err := c.cachedObjects(ctx, gvrEvents, namespace); ok && err == nil {
		items = make([]*corev1.Event, 0, len(objs))
		for _, o := range objs {
			if e, ok := o.(*corev1.Event); ok {
				items = append(items, e)
			}
		}
	} else {
		list, lerr := c.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
		if lerr != nil {
			return nil, lerr
		}
		items = make([]*corev1.Event, 0, len(list.Items))
		for i := range list.Items {
			items = append(items, &list.Items[i])
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return eventTime(items[i]).After(eventTime(items[j]))
	})
	out := make([]Event, 0, len(items))
	for _, e := range items {
		out = append(out, toEvent(e))
	}
	return out, nil
}

// eventTime returns an event's most recent effective timestamp, tolerating the
// newer events API that populates EventTime instead of LastTimestamp.
func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

// DeletePod deletes a pod by namespace and name.
func (c *Client) DeletePod(ctx context.Context, namespace, name string) error {
	return c.clientset.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// DeleteDeployment deletes a deployment by namespace and name.
func (c *Client) DeleteDeployment(ctx context.Context, namespace, name string) error {
	return c.clientset.AppsV1().Deployments(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// DeleteService deletes a service by namespace and name.
func (c *Client) DeleteService(ctx context.Context, namespace, name string) error {
	return c.clientset.CoreV1().Services(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// Delete dispatches deletion by kind. Nodes are intentionally excluded:
// deleting a Node object is destructive and rarely intended from a browser UI.
func (c *Client) Delete(ctx context.Context, kind, namespace, name string) error {
	switch kind {
	case "pod":
		return c.DeletePod(ctx, namespace, name)
	case "deployment":
		return c.DeleteDeployment(ctx, namespace, name)
	case "service":
		return c.DeleteService(ctx, namespace, name)
	case "configmap":
		return c.clientset.CoreV1().ConfigMaps(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	case "secret":
		return c.clientset.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	case "ingress":
		return c.clientset.NetworkingV1().Ingresses(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	case "job":
		// Delete dependent pods too, matching `kubectl delete job`.
		pol := metav1.DeletePropagationBackground
		return c.clientset.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &pol})
	case "statefulset":
		return c.clientset.AppsV1().StatefulSets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	case "pvc":
		return c.clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	case "daemonset":
		return c.clientset.AppsV1().DaemonSets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	case "replicaset":
		return c.clientset.AppsV1().ReplicaSets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	case "cronjob":
		return c.clientset.BatchV1().CronJobs(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	default:
		return fmt.Errorf("cannot delete kind %q", kind)
	}
}

// ScaleDeployment sets a deployment's replica count via the scale subresource.
func (c *Client) ScaleDeployment(ctx context.Context, namespace, name string, replicas int32) error {
	deps := c.clientset.AppsV1().Deployments(namespace)
	scale, err := deps.GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	scale.Spec.Replicas = replicas
	_, err = deps.UpdateScale(ctx, name, scale, metav1.UpdateOptions{})
	return err
}

// getObject fetches a typed object by kind with its TypeMeta (apiVersion/kind)
// populated, since typed Get calls leave TypeMeta empty. Shared by Describe and
// GetYAML; it masks/strips nothing — callers decide.
func (c *Client) getObject(ctx context.Context, kind, namespace, name string) (metav1.Object, error) {
	var obj metav1.Object
	var err error

	switch kind {
	case "pod":
		var p *corev1.Pod
		p, err = c.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if p != nil {
			p.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
		}
		obj = p
	case "deployment":
		var d *appsv1.Deployment
		d, err = c.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if d != nil {
			d.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
		}
		obj = d
	case "service":
		var s *corev1.Service
		s, err = c.clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if s != nil {
			s.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}
		}
		obj = s
	case "node":
		var n *corev1.Node
		n, err = c.clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if n != nil {
			n.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Node"}
		}
		obj = n
	case "configmap":
		var cm *corev1.ConfigMap
		cm, err = c.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		if cm != nil {
			cm.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
		}
		obj = cm
	case "secret":
		var s *corev1.Secret
		s, err = c.clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		if s != nil {
			s.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
		}
		obj = s
	case "ingress":
		var ing *networkingv1.Ingress
		ing, err = c.clientset.NetworkingV1().Ingresses(namespace).Get(ctx, name, metav1.GetOptions{})
		if ing != nil {
			ing.TypeMeta = metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"}
		}
		obj = ing
	case "job":
		var j *batchv1.Job
		j, err = c.clientset.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if j != nil {
			j.TypeMeta = metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}
		}
		obj = j
	case "statefulset":
		var s *appsv1.StatefulSet
		s, err = c.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if s != nil {
			s.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"}
		}
		obj = s
	case "pvc":
		var p *corev1.PersistentVolumeClaim
		p, err = c.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if p != nil {
			p.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
		}
		obj = p
	case "daemonset":
		var d *appsv1.DaemonSet
		d, err = c.clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if d != nil {
			d.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}
		}
		obj = d
	case "replicaset":
		var r *appsv1.ReplicaSet
		r, err = c.clientset.AppsV1().ReplicaSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if r != nil {
			r.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"}
		}
		obj = r
	case "cronjob":
		var cj *batchv1.CronJob
		cj, err = c.clientset.BatchV1().CronJobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if cj != nil {
			cj.TypeMeta = metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"}
		}
		obj = cj
	case "event":
		var e *corev1.Event
		e, err = c.clientset.CoreV1().Events(namespace).Get(ctx, name, metav1.GetOptions{})
		if e != nil {
			e.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Event"}
		}
		obj = e
	default:
		return nil, fmt.Errorf("unknown kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	return obj, nil
}

// Describe returns a YAML dump of the named object followed by its recent
// events, resembling `kubectl describe`. Secret values are masked.
func (c *Client) Describe(ctx context.Context, kind, namespace, name string) (string, error) {
	obj, err := c.getObject(ctx, kind, namespace, name)
	if err != nil {
		return "", err
	}
	if s, ok := obj.(*corev1.Secret); ok {
		maskSecret(s) // redact values before rendering
	}
	obj.SetManagedFields(nil) // drop noisy server-managed fields

	yml, err := yaml.Marshal(obj)
	if err != nil {
		return "", err
	}
	events, err := c.objectEvents(ctx, namespace, name)
	if err != nil {
		events = fmt.Sprintf("(failed to load events: %v)", err)
	}
	return fmt.Sprintf("%s\n--- Events ---\n%s", string(yml), events), nil
}

// GetYAML returns the object's YAML for editing: a fresh copy (carrying its
// resourceVersion, so the edit round-trips as an optimistic-locked update) and
// WITHOUT the events section. Secrets are intentionally NOT masked here — the
// real values must round-trip, or an apply would overwrite them with markers.
func (c *Client) GetYAML(ctx context.Context, kind, namespace, name string) (string, error) {
	obj, err := c.getObject(ctx, kind, namespace, name)
	if err != nil {
		return "", err
	}
	obj.SetManagedFields(nil)
	yml, err := yaml.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(yml), nil
}

// ApplyYAML updates a resource from edited YAML the way `kubectl edit` does — a
// PUT (Update), not server-side apply. It parses the YAML into an unstructured
// object, maps its kind to a REST resource via discovery, and updates through
// the dynamic client, so it works for any kind without a per-kind switch.
func (c *Client) ApplyYAML(ctx context.Context, data []byte) error {
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(data, &obj.Object); err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}
	gvk := obj.GroupVersionKind()
	if gvk.Kind == "" || obj.GetName() == "" {
		return fmt.Errorf("YAML is missing apiVersion/kind or metadata.name")
	}

	m, err := c.restMapper()
	if err != nil {
		return err
	}
	mapping, err := m.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("map %s: %w", gvk, err)
	}

	dyn, err := c.dynamicClient()
	if err != nil {
		return err
	}
	nri := dyn.Resource(mapping.Resource)
	var target dynamic.ResourceInterface = nri
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		target = nri.Namespace(obj.GetNamespace())
	}
	_, err = target.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

// maskSecret redacts a secret's values in place, keeping key names and byte
// lengths so the shape is visible without exposing the contents. It also drops
// the last-applied-configuration annotation, which can embed the raw values.
//
// The markers are written to StringData (not Data), because YAML marshals the
// []byte Data map as base64 — which would render the redaction markers as
// unreadable base64. StringData stays plain text.
func maskSecret(s *corev1.Secret) {
	masked := make(map[string]string, len(s.Data)+len(s.StringData))
	for k, v := range s.Data {
		masked[k] = fmt.Sprintf("<redacted: %d bytes>", len(v))
	}
	for k := range s.StringData {
		masked[k] = "<redacted>"
	}
	s.Data = nil
	s.StringData = masked
	delete(s.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
}

// objectEvents returns the events for a named object in the compact kubectl
// "TYPE  REASON  AGE  MESSAGE" style, oldest first.
func (c *Client) objectEvents(ctx context.Context, namespace, name string) (string, error) {
	sel := fields.OneTermEqualSelector("involvedObject.name", name).String()
	list, err := c.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: sel,
	})
	if err != nil {
		return "", err
	}
	if len(list.Items) == 0 {
		return "<none>\n", nil
	}

	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].LastTimestamp.Before(&list.Items[j].LastTimestamp)
	})

	var b strings.Builder
	for i := range list.Items {
		e := &list.Items[i]
		age := humanAge(e.LastTimestamp.Time)
		fmt.Fprintf(&b, "%-8s %-24s %-5s %s\n", e.Type, e.Reason, age, e.Message)
	}
	return b.String(), nil
}

// PodContainers returns the names of a pod's containers (init containers first,
// then regular), for the container picker.
func (c *Client) PodContainers(ctx context.Context, namespace, name string) ([]string, error) {
	p, err := c.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(p.Spec.InitContainers)+len(p.Spec.Containers))
	for _, ic := range p.Spec.InitContainers {
		names = append(names, ic.Name)
	}
	for _, con := range p.Spec.Containers {
		names = append(names, con.Name)
	}
	return names, nil
}

// PodLogs fetches the last tailLines log lines of the named container. An empty
// container lets the server pick (valid only for single-container pods).
func (c *Client) PodLogs(ctx context.Context, namespace, name, container string, tailLines int64) (string, error) {
	opts := &corev1.PodLogOptions{Container: container, TailLines: &tailLines}
	req := c.clientset.CoreV1().Pods(namespace).GetLogs(name, opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	buf := make([]byte, 0, 32*1024)
	tmp := make([]byte, 8*1024)
	for {
		n, err := stream.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break // io.EOF or transport close ends the tail
		}
	}
	return string(buf), nil
}

// StreamPodLogs opens a log stream for a container. With follow set the stream
// stays open and Read blocks until the context is cancelled; with previous set
// it returns the logs of the prior (crashed) container instance. The caller
// owns the returned stream and must Close it.
func (c *Client) StreamPodLogs(ctx context.Context, namespace, name, container string, follow, previous bool, tailLines int64) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{
		Container: container,
		Follow:    follow,
		Previous:  previous,
		TailLines: &tailLines,
	}
	return c.clientset.CoreV1().Pods(namespace).GetLogs(name, opts).Stream(ctx)
}

// toPod flattens a corev1.Pod into our display struct, mirroring the fields
// kubectl surfaces in `get pods`.
func toPod(p *corev1.Pod) Pod {
	ready, total := 0, len(p.Spec.Containers)
	var restarts int32
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		restarts += cs.RestartCount
	}

	status := string(p.Status.Phase)
	if p.DeletionTimestamp != nil {
		status = "Terminating"
	} else {
		// A waiting/terminated reason is more informative than the phase.
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
				status = cs.State.Waiting.Reason
				break
			}
			if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
				status = cs.State.Terminated.Reason
				break
			}
		}
	}

	return Pod{
		Name:      p.Name,
		Namespace: p.Namespace,
		Ready:     fmt.Sprintf("%d/%d", ready, total),
		Status:    status,
		Restarts:  restarts,
		CPU:       "-",
		Mem:       "-",
		Age:       humanAge(p.CreationTimestamp.Time),
		Node:      p.Spec.NodeName,
	}
}

// toPVC flattens a corev1.PersistentVolumeClaim, mirroring kubectl's columns.
func toPVC(p *corev1.PersistentVolumeClaim) PVC {
	capacity := ""
	if q, ok := p.Status.Capacity[corev1.ResourceStorage]; ok {
		capacity = q.String()
	}

	modes := make([]string, 0, len(p.Status.AccessModes))
	for _, m := range p.Status.AccessModes {
		switch m {
		case corev1.ReadWriteOnce:
			modes = append(modes, "RWO")
		case corev1.ReadOnlyMany:
			modes = append(modes, "ROX")
		case corev1.ReadWriteMany:
			modes = append(modes, "RWX")
		case corev1.ReadWriteOncePod:
			modes = append(modes, "RWOP")
		}
	}

	sc := "<none>"
	if p.Spec.StorageClassName != nil && *p.Spec.StorageClassName != "" {
		sc = *p.Spec.StorageClassName
	}

	return PVC{
		Name:         p.Name,
		Namespace:    p.Namespace,
		Status:       string(p.Status.Phase),
		Volume:       p.Spec.VolumeName,
		Capacity:     capacity,
		AccessModes:  strings.Join(modes, ","),
		StorageClass: sc,
		Age:          humanAge(p.CreationTimestamp.Time),
	}
}

// toIngress flattens a networkingv1.Ingress, mirroring kubectl's columns.
func toIngress(ing *networkingv1.Ingress) Ingress {
	class := "<none>"
	if ing.Spec.IngressClassName != nil && *ing.Spec.IngressClassName != "" {
		class = *ing.Spec.IngressClassName
	}

	hosts := make([]string, 0, len(ing.Spec.Rules))
	for _, r := range ing.Spec.Rules {
		if r.Host != "" {
			hosts = append(hosts, r.Host)
		}
	}
	hostStr := "*"
	if len(hosts) > 0 {
		hostStr = strings.Join(hosts, ",")
	}

	addrs := make([]string, 0, len(ing.Status.LoadBalancer.Ingress))
	for _, in := range ing.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			addrs = append(addrs, in.IP)
		} else if in.Hostname != "" {
			addrs = append(addrs, in.Hostname)
		}
	}

	ports := "80"
	if len(ing.Spec.TLS) > 0 {
		ports = "80,443"
	}

	return Ingress{
		Name:      ing.Name,
		Namespace: ing.Namespace,
		Class:     class,
		Hosts:     hostStr,
		Address:   strings.Join(addrs, ","),
		Ports:     ports,
		Age:       humanAge(ing.CreationTimestamp.Time),
	}
}

// toJob flattens a batchv1.Job, mirroring kubectl's COMPLETIONS/DURATION.
func toJob(j *batchv1.Job) Job {
	completions := "1"
	if j.Spec.Completions != nil {
		completions = fmt.Sprintf("%d", *j.Spec.Completions)
	}

	duration := "<none>"
	if j.Status.StartTime != nil {
		end := time.Now()
		if j.Status.CompletionTime != nil {
			end = j.Status.CompletionTime.Time
		}
		duration = humanDuration(end.Sub(j.Status.StartTime.Time))
	}

	return Job{
		Name:        j.Name,
		Namespace:   j.Namespace,
		Completions: fmt.Sprintf("%d/%s", j.Status.Succeeded, completions),
		Duration:    duration,
		Age:         humanAge(j.CreationTimestamp.Time),
	}
}

// humanDuration renders a duration in the compact kubectl style (5s, 3m, 2h).
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// toNode flattens a corev1.Node into our display struct, mirroring the
// STATUS/ROLES/VERSION columns kubectl shows.
func toNode(n *corev1.Node) Node {
	status := "NotReady"
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			if cond.Status == corev1.ConditionTrue {
				status = "Ready"
			}
			break
		}
	}
	if n.Spec.Unschedulable {
		status += ",SchedulingDisabled"
	}

	// Roles come from node-role.kubernetes.io/<role> labels.
	roles := make([]string, 0, 2)
	for label := range n.Labels {
		if r, ok := strings.CutPrefix(label, "node-role.kubernetes.io/"); ok && r != "" {
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	roleStr := "<none>"
	if len(roles) > 0 {
		roleStr = strings.Join(roles, ",")
	}

	return Node{
		Name:        n.Name,
		Status:      status,
		Roles:       roleStr,
		Version:     n.Status.NodeInfo.KubeletVersion,
		CPU:         "-",
		Mem:         "-",
		Age:         humanAge(n.CreationTimestamp.Time),
		cpuCapMilli: n.Status.Capacity.Cpu().MilliValue(),
		memCapBytes: n.Status.Capacity.Memory().Value(),
	}
}

// toDeployment flattens an appsv1.Deployment into our display struct.
func toDeployment(d *appsv1.Deployment) Deployment {
	desired := int32(0)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	return Deployment{
		Name:      d.Name,
		Namespace: d.Namespace,
		Ready:     fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, desired),
		Desired:   desired,
		UpToDate:  d.Status.UpdatedReplicas,
		Available: d.Status.AvailableReplicas,
		Age:       humanAge(d.CreationTimestamp.Time),
	}
}

// toService flattens a corev1.Service into our display struct.
func toService(s *corev1.Service) Service {
	ports := make([]string, 0, len(s.Spec.Ports))
	for _, p := range s.Spec.Ports {
		ports = append(ports, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
	}

	external := externalIP(s)
	return Service{
		Name:       s.Name,
		Namespace:  s.Namespace,
		Type:       string(s.Spec.Type),
		ClusterIP:  s.Spec.ClusterIP,
		ExternalIP: external,
		Ports:      strings.Join(ports, ","),
		Age:        humanAge(s.CreationTimestamp.Time),
	}
}

// externalIP mirrors how kubectl derives the EXTERNAL-IP column.
func externalIP(s *corev1.Service) string {
	switch s.Spec.Type {
	case corev1.ServiceTypeExternalName:
		return s.Spec.ExternalName
	case corev1.ServiceTypeLoadBalancer:
		ips := make([]string, 0, len(s.Status.LoadBalancer.Ingress))
		for _, ing := range s.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				ips = append(ips, ing.IP)
			} else if ing.Hostname != "" {
				ips = append(ips, ing.Hostname)
			}
		}
		if len(ips) == 0 {
			return "<pending>"
		}
		return strings.Join(ips, ",")
	default:
		if len(s.Spec.ExternalIPs) > 0 {
			return strings.Join(s.Spec.ExternalIPs, ",")
		}
		return "<none>"
	}
}

// sortByNsName sorts n items by (namespace, name) using key/swap closures,
// shared by the resource listers to keep table order stable.
func sortByNsName(n int, key func(i int) (string, string), swap func(i, j int)) {
	sort.Sort(nsNameSorter{n: n, key: key, swapFn: swap})
}

type nsNameSorter struct {
	n      int
	key    func(i int) (string, string)
	swapFn func(i, j int)
}

func (s nsNameSorter) Len() int      { return s.n }
func (s nsNameSorter) Swap(i, j int) { s.swapFn(i, j) }
func (s nsNameSorter) Less(i, j int) bool {
	ni, mi := s.key(i)
	nj, mj := s.key(j)
	if ni != nj {
		return ni < nj
	}
	return mi < mj
}

// humanAge renders a duration since t in the compact kubectl style (5m, 3h, 2d).
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
