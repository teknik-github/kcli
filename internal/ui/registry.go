package ui

import (
	"context"
	"time"

	"github.com/teknik-github/kcli/internal/k8s"
)

// Row is the uniform, display-ready shape every resource is flattened into.
// Namespace/Name drive selection and actions; Cells fill the table columns in
// the same order as viewDef.Columns.
type Row struct {
	Namespace string
	Name      string
	Cells     []string
}

// viewCaps declares which actions a view supports, replacing per-key view
// guards with a data-driven check.
type viewCaps struct {
	Logs        bool
	Exec        bool
	Scale       bool
	Graph       bool
	Delete      bool
	PortForward bool
	Restart     bool // rollout restart (workloads with a pod template)
	Cordon      bool // toggle node schedulability
	Drain       bool // cordon + evict pods (nodes)
}

// viewDef describes one resource view. Adding a resource means appending one
// viewDef to resourceViews — no other switch statements to touch.
type viewDef struct {
	ID              string // singular kind, used by Describe/Delete ("pod", ...)
	Title           string
	Columns         []string
	StatusCol       int           // column index to color as a status, -1 for none
	ClusterScoped   bool          // no namespace (nodes)
	Local           bool          // rows come from App state, not the cluster (Fetch unused)
	RefreshInterval time.Duration // per-view auto-refresh cadence; 0 = default (refreshInterval)
	Caps            viewCaps
	Fetch           func(ctx context.Context, c *k8s.Client, namespace string) ([]Row, error)
}

// resourceViews is the single source of truth for the tab bar and all
// per-resource behaviour. Order defines tab order and the 1..N number keys.
var resourceViews = []*viewDef{
	{
		ID:        "pod",
		Title:     "Pods",
		Columns:   []string{"NAMESPACE", "NAME", "READY", "STATUS", "RESTARTS", "CPU", "MEM", "AGE", "NODE"},
		StatusCol: 3,
		Caps:      viewCaps{Logs: true, Exec: true, Graph: true, Delete: true, PortForward: true},
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			pods, err := c.Pods(ctx, ns)
			if err != nil {
				return nil, err
			}
			c.AddPodMetrics(ctx, pods) // best-effort CPU/MEM
			rows := make([]Row, len(pods))
			for i, p := range pods {
				rows[i] = Row{p.Namespace, p.Name, []string{p.Namespace, p.Name, p.Ready,
					p.Status, itoa(int(p.Restarts)), p.CPU, p.Mem, p.Age, p.Node}}
			}
			return rows, nil
		},
	},
	{
		ID:        "deployment",
		Title:     "Deployments",
		Columns:   []string{"NAMESPACE", "NAME", "READY", "UP-TO-DATE", "AVAILABLE", "AGE"},
		StatusCol: -1,
		Caps:      viewCaps{Scale: true, Delete: true, Restart: true},
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			deps, err := c.Deployments(ctx, ns)
			if err != nil {
				return nil, err
			}
			rows := make([]Row, len(deps))
			for i, d := range deps {
				rows[i] = Row{d.Namespace, d.Name, []string{d.Namespace, d.Name, d.Ready,
					itoa(int(d.UpToDate)), itoa(int(d.Available)), d.Age}}
			}
			return rows, nil
		},
	},
	{
		ID:        "daemonset",
		Title:     "DaemonSets",
		Columns:   []string{"NAMESPACE", "NAME", "DESIRED", "CURRENT", "READY", "UP-TO-DATE", "AVAILABLE", "AGE"},
		StatusCol: -1,
		Caps:      viewCaps{Restart: true, Delete: true},
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			sets, err := c.DaemonSets(ctx, ns)
			if err != nil {
				return nil, err
			}
			rows := make([]Row, len(sets))
			for i, d := range sets {
				rows[i] = Row{d.Namespace, d.Name, []string{d.Namespace, d.Name,
					itoa(int(d.Desired)), itoa(int(d.Current)), itoa(int(d.Ready)),
					itoa(int(d.UpToDate)), itoa(int(d.Available)), d.Age}}
			}
			return rows, nil
		},
	},
	{
		ID:        "service",
		Title:     "Services",
		Columns:   []string{"NAMESPACE", "NAME", "TYPE", "CLUSTER-IP", "EXTERNAL-IP", "PORTS", "AGE"},
		StatusCol: -1,
		Caps:      viewCaps{Delete: true},
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			svcs, err := c.Services(ctx, ns)
			if err != nil {
				return nil, err
			}
			rows := make([]Row, len(svcs))
			for i, s := range svcs {
				rows[i] = Row{s.Namespace, s.Name, []string{s.Namespace, s.Name, s.Type,
					s.ClusterIP, s.ExternalIP, s.Ports, s.Age}}
			}
			return rows, nil
		},
	},
	{
		ID:            "node",
		Title:         "Nodes",
		Columns:       []string{"NAME", "STATUS", "ROLES", "CPU", "MEM", "VERSION", "AGE"},
		StatusCol:     1,
		ClusterScoped: true,
		Caps:          viewCaps{Graph: true, Cordon: true, Drain: true}, // nodes are never deleted from kcli
		Fetch: func(ctx context.Context, c *k8s.Client, _ string) ([]Row, error) {
			nodes, err := c.Nodes(ctx)
			if err != nil {
				return nil, err
			}
			c.AddNodeMetrics(ctx, nodes)
			rows := make([]Row, len(nodes))
			for i, n := range nodes {
				rows[i] = Row{"", n.Name, []string{n.Name, n.Status, n.Roles,
					n.CPU, n.Mem, n.Version, n.Age}}
			}
			return rows, nil
		},
	},
	{
		ID:        "statefulset",
		Title:     "StatefulSets",
		Columns:   []string{"NAMESPACE", "NAME", "READY", "AGE"},
		StatusCol: -1,
		Caps:      viewCaps{Scale: true, Delete: true, Restart: true},
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			sets, err := c.StatefulSets(ctx, ns)
			if err != nil {
				return nil, err
			}
			rows := make([]Row, len(sets))
			for i, s := range sets {
				rows[i] = Row{s.Namespace, s.Name, []string{s.Namespace, s.Name, s.Ready, s.Age}}
			}
			return rows, nil
		},
	},
	{
		ID:        "replicaset",
		Title:     "ReplicaSets",
		Columns:   []string{"NAMESPACE", "NAME", "DESIRED", "CURRENT", "READY", "AGE"},
		StatusCol: -1,
		Caps:      viewCaps{Delete: true},
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			sets, err := c.ReplicaSets(ctx, ns)
			if err != nil {
				return nil, err
			}
			rows := make([]Row, len(sets))
			for i, r := range sets {
				rows[i] = Row{r.Namespace, r.Name, []string{r.Namespace, r.Name,
					itoa(int(r.Desired)), itoa(int(r.Current)), itoa(int(r.Ready)), r.Age}}
			}
			return rows, nil
		},
	},
	{
		ID:        "pvc",
		Title:     "PVCs",
		Columns:   []string{"NAMESPACE", "NAME", "STATUS", "VOLUME", "CAPACITY", "ACCESS", "STORAGECLASS", "AGE"},
		StatusCol: 2,
		Caps:      viewCaps{Delete: true},
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			pvcs, err := c.PVCs(ctx, ns)
			if err != nil {
				return nil, err
			}
			rows := make([]Row, len(pvcs))
			for i, p := range pvcs {
				rows[i] = Row{p.Namespace, p.Name, []string{p.Namespace, p.Name, p.Status,
					p.Volume, p.Capacity, p.AccessModes, p.StorageClass, p.Age}}
			}
			return rows, nil
		},
	},
	{
		ID:        "ingress",
		Title:     "Ingresses",
		Columns:   []string{"NAMESPACE", "NAME", "CLASS", "HOSTS", "ADDRESS", "PORTS", "AGE"},
		StatusCol: -1,
		Caps:      viewCaps{Delete: true},
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			ings, err := c.Ingresses(ctx, ns)
			if err != nil {
				return nil, err
			}
			rows := make([]Row, len(ings))
			for i, in := range ings {
				rows[i] = Row{in.Namespace, in.Name, []string{in.Namespace, in.Name,
					in.Class, in.Hosts, in.Address, in.Ports, in.Age}}
			}
			return rows, nil
		},
	},
	{
		ID:        "job",
		Title:     "Jobs",
		Columns:   []string{"NAMESPACE", "NAME", "COMPLETIONS", "DURATION", "AGE"},
		StatusCol: -1,
		Caps:      viewCaps{Delete: true},
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			jobs, err := c.Jobs(ctx, ns)
			if err != nil {
				return nil, err
			}
			rows := make([]Row, len(jobs))
			for i, j := range jobs {
				rows[i] = Row{j.Namespace, j.Name, []string{j.Namespace, j.Name,
					j.Completions, j.Duration, j.Age}}
			}
			return rows, nil
		},
	},
	{
		ID:        "cronjob",
		Title:     "CronJobs",
		Columns:   []string{"NAMESPACE", "NAME", "SCHEDULE", "SUSPEND", "ACTIVE", "LAST-SCHEDULE", "AGE"},
		StatusCol: -1,
		Caps:      viewCaps{Delete: true},
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			jobs, err := c.CronJobs(ctx, ns)
			if err != nil {
				return nil, err
			}
			rows := make([]Row, len(jobs))
			for i, cj := range jobs {
				rows[i] = Row{cj.Namespace, cj.Name, []string{cj.Namespace, cj.Name,
					cj.Schedule, boolStr(cj.Suspend), itoa(cj.Active), cj.LastSchedule, cj.Age}}
			}
			return rows, nil
		},
	},
	{
		ID:        "configmap",
		Title:     "ConfigMaps",
		Columns:   []string{"NAMESPACE", "NAME", "DATA", "AGE"},
		StatusCol: -1,
		Caps:      viewCaps{Delete: true},
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			cms, err := c.ConfigMaps(ctx, ns)
			if err != nil {
				return nil, err
			}
			rows := make([]Row, len(cms))
			for i, cm := range cms {
				rows[i] = Row{cm.Namespace, cm.Name, []string{cm.Namespace, cm.Name,
					itoa(cm.Data), cm.Age}}
			}
			return rows, nil
		},
	},
	{
		ID:        "secret",
		Title:     "Secrets",
		Columns:   []string{"NAMESPACE", "NAME", "TYPE", "DATA", "AGE"},
		StatusCol: -1,
		Caps:      viewCaps{Delete: true},
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			secs, err := c.Secrets(ctx, ns)
			if err != nil {
				return nil, err
			}
			rows := make([]Row, len(secs))
			for i, s := range secs {
				rows[i] = Row{s.Namespace, s.Name, []string{s.Namespace, s.Name, s.Type,
					itoa(s.Data), s.Age}}
			}
			return rows, nil
		},
	},
	{
		ID:              "event",
		Title:           "Events",
		Columns:         []string{"NAMESPACE", "LAST-SEEN", "TYPE", "REASON", "OBJECT", "COUNT", "MESSAGE"},
		StatusCol:       2,                // TYPE: Normal (green) / Warning (red)
		RefreshInterval: 15 * time.Second, // events can be numerous; poll them less often
		Caps:            viewCaps{},       // read-only; Enter still opens the event YAML
		Fetch: func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
			evs, err := c.Events(ctx, ns)
			if err != nil {
				return nil, err
			}
			rows := make([]Row, len(evs))
			for i, e := range evs {
				rows[i] = Row{e.Namespace, e.Name, []string{e.Namespace, e.LastSeen,
					e.Type, e.Reason, e.Object, itoa(int(e.Count)), e.Message}}
			}
			return rows, nil
		},
	},
	{
		ID:        "portforward",
		Title:     "Port-Fwd",
		Columns:   []string{"ID", "NAMESPACE", "POD", "PORTS", "STATUS"},
		StatusCol: 4,
		Local:     true, // rows built from App.forwards, not the cluster
	},
}
