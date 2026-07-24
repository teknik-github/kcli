package ui

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/teknik-github/kcli/internal/bench"
	"github.com/teknik-github/kcli/internal/k8s"
)

// The Bench view is an app-local list of HTTP load tests, one row per run. A
// test against a Pod or Service goes through an ephemeral port-forward, so it
// measures the workload itself; a test against an Ingress goes straight over
// the network, so it measures the whole ingress path.

// init wires the Bench view's app-local behaviour, for the same reason
// portforward.go and pulse.go do it here: these closures reach resourceViews
// through App, so naming them in the registry literal is an initialization
// cycle.
func init() {
	if v, ok := viewByID("bench"); ok {
		v.LocalRows = (*App).benchRows
		v.LocalHint = "enter report · d stop/clear · q back"
		v.OnEnter = (*App).showBenchReport
		v.OnDelete = (*App).dropSelectedBench
	}
}

// benchRun is one load test. Every field is read and written on the UI
// goroutine only (background work reports in through QueueUpdateDraw), so it
// needs no locking.
type benchRun struct {
	id           int
	target       string // "service/default/nginx", shown in the TARGET column
	url          string // what is actually being hit (the local forward for in-cluster targets)
	note         string // how the target was reached, for the report
	requests     int
	concurrency  int
	done, failed int    // live progress
	status       string // starting / running / done / cancelled / error: …
	started      time.Time
	res          *bench.Result
	cancel       context.CancelFunc
}

// benchDefaults are the dialog's starting values — a short run that finishes
// quickly enough to iterate on.
const (
	benchDefaultRequests    = 200
	benchDefaultConcurrency = 10
	benchReadyTimeout       = 15 * time.Second // how long to wait for the ephemeral forward
)

// showBenchDialog prompts for the load-test parameters for the selected Pod,
// Service, or Ingress and starts the run in the background.
func (a *App) showBenchDialog() {
	row, ok := a.selectedRow()
	if !ok {
		return
	}
	kind, ns, name := a.view().ID, row.Namespace, row.Name

	// An Ingress publishes its own port (80, or 443 when it terminates TLS), so
	// leave it blank and resolve it from the object; a Pod/Service needs one.
	portText := "80"
	portLabel := "Port: "
	if kind == "ingress" {
		portText, portLabel = "", "Port (blank = auto): "
	}
	path := tview.NewInputField().SetLabel("Path: ").SetText("/").SetFieldWidth(24)
	port := tview.NewInputField().SetLabel(portLabel).SetText(portText).SetFieldWidth(8)
	reqs := tview.NewInputField().SetLabel("Requests: ").SetText(itoa(benchDefaultRequests)).SetFieldWidth(8)
	conc := tview.NewInputField().SetLabel("Concurrency: ").SetText(itoa(benchDefaultConcurrency)).SetFieldWidth(8)

	form := tview.NewForm().
		AddFormItem(path).AddFormItem(port).AddFormItem(reqs).AddFormItem(conc)
	form.AddButton("Run", func() {
		p, err := atoiField(port.GetText(), 0) // 0 = auto (Ingress only)
		if err != nil {
			a.showMessage("bench", "port: "+err.Error())
			return
		}
		n, err := atoiField(reqs.GetText(), 0)
		if err != nil || n < 1 {
			a.showMessage("bench", "requests must be a positive number")
			return
		}
		c, err := atoiField(conc.GetText(), 0)
		if err != nil || c < 1 {
			a.showMessage("bench", "concurrency must be a positive number")
			return
		}
		if kind != "ingress" && p < 1 {
			a.showMessage("bench", "a pod or service needs a target port")
			return
		}
		a.closeModal("bench-dialog")
		a.startBench(kind, ns, name, strings.TrimSpace(path.GetText()), p, n, c)
	})
	form.AddButton("Cancel", func() { a.closeModal("bench-dialog") })
	form.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape {
			a.closeModal("bench-dialog")
			return nil
		}
		return e
	})
	form.SetBorder(true).SetTitle(fmt.Sprintf(" benchmark %s/%s ", kind, name))

	a.openModal("bench-dialog", form, 52, 11)
}

// startBench registers the run and kicks it off in the background. Runs on the
// UI goroutine; the client is captured here so a later context switch cannot
// redirect the test to another cluster.
func (a *App) startBench(kind, ns, name, path string, port, requests, concurrency int) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &benchRun{
		id:          a.nextBenchID,
		target:      fmt.Sprintf("%s/%s/%s", kind, ns, name),
		requests:    requests,
		concurrency: concurrency,
		status:      "starting",
		started:     time.Now(),
		cancel:      cancel,
	}
	if ns == "" {
		b.target = fmt.Sprintf("%s/%s", kind, name)
	}
	a.nextBenchID++
	a.benches = append(a.benches, b)
	a.redrawBench()

	cl := a.client
	go a.runBench(ctx, b, cl, kind, ns, name, path, port)

	a.showMessage("bench", fmt.Sprintf("benchmark #%d started: %d requests, concurrency %d.\n\nPress B for the Bench view.",
		b.id, requests, concurrency))
}

// runBench resolves the target, runs the load test, and reports back. Entirely
// on a background goroutine: every UI touch goes through QueueUpdateDraw.
func (a *App) runBench(ctx context.Context, b *benchRun, cl *k8s.Client, kind, ns, name, path string, port int) {
	dest, err := benchTarget(ctx, cl, kind, ns, name, path, port)
	if dest.stop != nil {
		defer dest.stop()
	}
	if err != nil {
		a.tv.QueueUpdateDraw(func() {
			b.status = "error: " + firstLine(err.Error())
			a.redrawBench()
		})
		return
	}

	a.tv.QueueUpdateDraw(func() {
		b.url, b.note, b.status = dest.url, dest.note, "running"
		b.started = time.Now()
		a.redrawBench()
	})

	opts := bench.Options{
		URL:         dest.url,
		Host:        dest.host,
		Requests:    b.requests,
		Concurrency: b.concurrency,
		Insecure:    strings.HasPrefix(dest.url, "https://"), // cluster ingresses are routinely self-signed
	}
	res, err := bench.Run(ctx, opts, func(done, failed int) {
		a.tv.QueueUpdateDraw(func() {
			b.done, b.failed = done, failed
			a.redrawBench()
		})
	})
	a.tv.QueueUpdateDraw(func() {
		switch {
		case err != nil:
			b.status = "error: " + firstLine(err.Error())
		case ctx.Err() != nil:
			b.status = "cancelled"
		default:
			b.status = "done"
		}
		if res != nil {
			b.res = res
			b.done, b.failed = res.Total, res.Failed
		}
		a.redrawBench()
	})
}

// benchDest is where a run's traffic actually goes: the URL to hit, the Host
// header that selects an ingress rule, a human note for the report, and the
// teardown for any ephemeral port-forward opened to get there.
type benchDest struct {
	url  string
	host string
	note string
	stop func()
}

// benchTarget turns a cluster object into a URL an HTTP client can hit. Pods
// and Services are reached through an ephemeral port-forward (torn down by
// dest.stop); an Ingress is hit directly, since being reachable from outside is
// its whole point.
func benchTarget(ctx context.Context, cl *k8s.Client, kind, ns, name, path string, port int) (benchDest, error) {
	if kind == "ingress" {
		addr, host, secure, err := cl.IngressTarget(ctx, ns, name)
		if err != nil {
			return benchDest{}, err
		}
		scheme, p := "http", 80
		if secure {
			scheme, p = "https", 443
		}
		if port > 0 {
			p = port
		}
		note := fmt.Sprintf("direct to ingress %s:%d", addr, p)
		if host != "" {
			note += fmt.Sprintf(" (Host: %s)", host)
		}
		return benchDest{
			url:  fmt.Sprintf("%s://%s:%d%s", scheme, addr, p, path),
			host: host,
			note: note,
		}, nil
	}

	local, err := freeLocalPort()
	if err != nil {
		return benchDest{}, fmt.Errorf("pick a local port: %w", err)
	}
	pod := name
	ports := []string{fmt.Sprintf("%d:%d", local, port)}
	if kind == "service" {
		// Follow targetPort to the pod's real container port, exactly as a manual
		// port-forward against a Service would.
		pod, ports, err = cl.ServiceForward(ctx, ns, name, ports)
		if err != nil {
			return benchDest{}, err
		}
	}

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		// The forward's own notices are noise here — the benchmark reports the
		// connection errors that matter.
		errCh <- cl.PortForward(ns, pod, ports, io.Discard, io.Discard, stopCh, readyCh)
	}()
	closeOnce := func() {
		select {
		case <-stopCh: // already closed
		default:
			close(stopCh)
		}
	}
	select {
	case <-readyCh:
	case err := <-errCh:
		return benchDest{}, fmt.Errorf("port-forward: %w", err)
	case <-ctx.Done():
		closeOnce()
		return benchDest{}, ctx.Err()
	case <-time.After(benchReadyTimeout):
		closeOnce()
		return benchDest{}, fmt.Errorf("port-forward to %s did not become ready", pod)
	}

	remote := ports[0]
	if _, r, found := strings.Cut(ports[0], ":"); found {
		remote = r
	}
	return benchDest{
		url:  fmt.Sprintf("http://127.0.0.1:%d%s", local, path),
		note: fmt.Sprintf("via port-forward 127.0.0.1:%d -> pod %s:%s", local, pod, remote),
		stop: closeOnce,
	}, nil
}

// freeLocalPort asks the kernel for an unused port. It is released again before
// the forward binds it, which is the usual (and inherently racy) way to do
// this; a collision surfaces as a port-forward error on the run's own row.
func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// benchRows renders the runs for the Bench view. The ID column keys back to the
// *benchRun across filter and sort.
func (a *App) benchRows() []Row {
	rows := make([]Row, len(a.benches))
	for i, b := range a.benches {
		rps, p95 := "-", "-"
		if b.res != nil {
			rps = fmt.Sprintf("%.0f", b.res.RPS)
			p95 = fmtDur(b.res.P95)
		}
		rows[i] = Row{
			Namespace: "",
			Name:      b.target,
			Cells: []string{itoa(b.id), b.target,
				fmt.Sprintf("%d/%d", b.done, b.requests), itoa(b.concurrency),
				rps, p95, itoa(b.done - b.failed), itoa(b.failed), b.status},
		}
	}
	return rows
}

// redrawBench keeps the header count and, when the Bench view is on screen, its
// rows current — progress arrives faster than the refresh tick.
func (a *App) redrawBench() {
	a.drawHeader()
	if a.view().ID == "bench" {
		a.rows = a.benchRows()
		a.drawTable()
	}
}

// gotoBenchView switches to the Bench view (bound to 'B'), remembering where it
// was called from.
func (a *App) gotoBenchView() { a.gotoLocalView("bench") }

// selectedBench returns the run under the cursor, keyed by the ID column so it
// survives filtering and sorting.
func (a *App) selectedBench() *benchRun {
	row, ok := a.selectedRow()
	if !ok || len(row.Cells) == 0 {
		return nil
	}
	id, err := strconv.Atoi(row.Cells[0])
	if err != nil {
		return nil
	}
	for _, b := range a.benches {
		if b.id == id {
			return b
		}
	}
	return nil
}

// dropSelectedBench cancels the run under the cursor (if it is still going) and
// drops it from the list — one key for both "stop this" and "clear this".
func (a *App) dropSelectedBench() {
	b := a.selectedBench()
	if b == nil {
		return
	}
	if b.cancel != nil {
		b.cancel()
	}
	for i, r := range a.benches {
		if r == b {
			a.benches = append(a.benches[:i], a.benches[i+1:]...)
			break
		}
	}
	a.redrawBench()
}

// showBenchReport opens the full result for the run under the cursor: the
// throughput and latency summary, the status-code and error breakdowns, and a
// latency distribution.
func (a *App) showBenchReport() {
	b := a.selectedBench()
	if b == nil {
		return
	}
	view := tview.NewTextView().SetScrollable(true).SetWrap(false)
	view.SetBorder(true).SetTitle(fmt.Sprintf(" benchmark #%d  %s   q/esc close ", b.id, b.target))
	view.SetText(benchReport(b))
	view.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape || e.Rune() == 'q' {
			a.closeModal("bench-report")
			return nil
		}
		return e
	})
	a.openModalFull("bench-report", view)
}

// benchReport formats one run as plain text.
func benchReport(b *benchRun) string {
	var s strings.Builder
	fmt.Fprintf(&s, "target       %s\n", b.target)
	if b.url != "" {
		fmt.Fprintf(&s, "url          %s\n", b.url)
	}
	if b.note != "" {
		fmt.Fprintf(&s, "reached      %s\n", b.note)
	}
	fmt.Fprintf(&s, "load         %d requests, concurrency %d\n", b.requests, b.concurrency)
	fmt.Fprintf(&s, "started      %s\n", b.started.Format("15:04:05"))
	fmt.Fprintf(&s, "status       %s\n", b.status)

	r := b.res
	if r == nil {
		fmt.Fprintf(&s, "progress     %d/%d done, %d failed\n", b.done, b.requests, b.failed)
		return s.String()
	}

	fmt.Fprintf(&s, "\nsummary\n")
	fmt.Fprintf(&s, "  elapsed    %s\n", fmtDur(r.Elapsed))
	fmt.Fprintf(&s, "  throughput %.1f req/s\n", r.RPS)
	fmt.Fprintf(&s, "  transfer   %s\n", humanBytes(r.Bytes))
	fmt.Fprintf(&s, "  requests   %d ok, %d failed\n", r.Success, r.Failed)

	fmt.Fprintf(&s, "\nlatency\n")
	for _, l := range []struct {
		name string
		d    time.Duration
	}{{"min", r.Min}, {"mean", r.Mean}, {"p50", r.P50}, {"p90", r.P90},
		{"p95", r.P95}, {"p99", r.P99}, {"max", r.Max}} {
		fmt.Fprintf(&s, "  %-10s %s\n", l.name, fmtDur(l.d))
	}

	fmt.Fprintf(&s, "\nstatus codes\n")
	if len(r.Codes) == 0 {
		fmt.Fprintf(&s, "  (none — every request failed before a response)\n")
	}
	codes := make([]int, 0, len(r.Codes))
	for c := range r.Codes {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	for _, c := range codes {
		fmt.Fprintf(&s, "  %-10d %d\n", c, r.Codes[c])
	}

	if len(r.Errors) > 0 {
		fmt.Fprintf(&s, "\nerrors\n")
		type kv struct {
			msg string
			n   int
		}
		list := make([]kv, 0, len(r.Errors))
		for m, n := range r.Errors {
			list = append(list, kv{m, n})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].n != list[j].n {
				return list[i].n > list[j].n
			}
			return list[i].msg < list[j].msg
		})
		for _, e := range list {
			fmt.Fprintf(&s, "  %-5d %s\n", e.n, e.msg)
		}
	}

	if h := r.Histogram(10); len(h) > 0 {
		max := 0
		for _, bk := range h {
			if bk.Count > max {
				max = bk.Count
			}
		}
		fmt.Fprintf(&s, "\nlatency distribution\n")
		for _, bk := range h {
			bar := 0
			if max > 0 {
				bar = bk.Count * 40 / max
			}
			fmt.Fprintf(&s, "  %-9s %s %d\n", fmtDur(bk.Hi), strings.Repeat("█", bar), bk.Count)
		}
	}
	return s.String()
}

// atoiField parses a numeric form field, returning def when it is left blank.
func atoiField(s string, def int) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return n, nil
}

// fmtDur renders a latency at a readable scale.
func fmtDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

// humanBytes renders a byte count in the usual binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
