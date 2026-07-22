package ui

import (
	"context"
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// maxTailPods caps how many pods a multi-tail follows at once: each one holds an
// open log stream against the API server, so an unbounded fan-out on a large
// namespace would be hostile. Extra pods are dropped and reported in the title.
const maxTailPods = 20

// multiTailLines is the per-pod backlog requested when the tail starts. Kept
// small because the buffer is shared across pods (logMaxLines total).
const multiTailLines = 100

// podPalette colours each pod's prefix so interleaved lines stay attributable.
// Cycled by pod index; tview colour names, deliberately avoiding red (reserved
// for error lines) and gray (used for status text).
var podPalette = []string{"aqua", "lime", "yellow", "fuchsia", "teal", "olive", "blue", "purple", "green", "silver"}

// mline is one buffered log line together with the pod it came from, so the
// grep filter and the colour prefix can both be applied at render time.
type mline struct {
	pod  string
	text string
	err  bool // stream/lookup failure, rendered red
}

// multiLogState is the multi-pod counterpart of logState: a rolling buffer of
// attributed lines plus the active grep. Like logState, every field is touched
// only on the UI goroutine, so grep re-renders the same buffer without
// re-streaming.
type multiLogState struct {
	view   *tview.TextView
	lines  []mline
	grep   string
	colors map[string]string // pod label -> tview colour name
	width  int               // prefix column width
}

// render repaints the pane, prefixing every line with its (coloured) pod label
// and applying the grep filter to the label + text.
func (st *multiLogState) render() {
	var b strings.Builder
	shown := 0
	g := strings.ToLower(st.grep)
	for _, ln := range st.lines {
		if g != "" && !strings.Contains(strings.ToLower(ln.pod+" "+ln.text), g) {
			continue
		}
		fmt.Fprintf(&b, "[%s]%-*s[-] ", st.colors[ln.pod], st.width, truncLabel(ln.pod, st.width))
		if ln.err {
			b.WriteString("[red]")
		}
		b.WriteString(tview.Escape(ln.text))
		if ln.err {
			b.WriteString("[-]")
		}
		b.WriteByte('\n')
		shown++
	}
	switch {
	case len(st.lines) == 0:
		st.view.SetText("[gray]waiting for output…[-]")
	case shown == 0:
		st.view.SetText("[gray](no lines match the grep)[-]")
	default:
		st.view.SetText(b.String())
		st.view.ScrollToEnd()
	}
}

// showMultiLogs tails several pods at once: the marked rows if any, otherwise
// every row currently visible (i.e. after the active filter). Bound to 'L' in
// Logs-capable views.
func (a *App) showMultiLogs() {
	targets := a.tailTargets()
	if len(targets) == 0 {
		return
	}
	total := len(targets)
	if len(targets) > maxTailPods {
		targets = targets[:maxTailPods]
	}
	a.openMultiLogs(targets, total)
}

// tailTargets picks the pods to follow, keeping the on-screen order so the
// colour assignment matches what the table shows.
func (a *App) tailTargets() []Row {
	rows := a.filteredRows()
	if len(a.marked) == 0 {
		return rows
	}
	out := make([]Row, 0, len(a.marked))
	for _, r := range rows {
		if a.marked[rowKey(r)] {
			out = append(out, r)
		}
	}
	return out
}

// openMultiLogs opens the tail pane and starts one follow stream per pod. Inside
// the pane '/' greps the buffer and q/esc close (stopping every stream).
func (a *App) openMultiLogs(targets []Row, total int) {
	view := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(false)
	view.SetBorder(true)

	st := &multiLogState{view: view, colors: map[string]string{}}
	labels := tailLabels(targets)
	for i, l := range labels {
		st.colors[l] = podPalette[i%len(podPalette)]
		if len(l) > st.width {
			st.width = len(l)
		}
	}
	if st.width > 28 {
		st.width = 28
	}

	setTitle := func() {
		scope := fmt.Sprintf("%d pods", len(targets))
		if total > len(targets) {
			scope = fmt.Sprintf("%d of %d pods", len(targets), total)
		}
		grep := ""
		if st.grep != "" {
			grep = fmt.Sprintf("  grep:%q", st.grep)
		}
		view.SetTitle(fmt.Sprintf(" tail: %s%s  /:grep  q:close ", scope, grep))
	}
	setTitle()
	view.SetText("[gray]streaming…[-]")

	view.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		switch {
		case e.Key() == tcell.KeyEscape || e.Rune() == 'q':
			a.closeMultiLogs()
			return nil
		case e.Rune() == '/':
			a.showMultiLogGrep(st, setTitle)
			return nil
		}
		return e
	})

	a.openModalFull("multilog", view)
	a.startMultiTail(st, targets, labels)
}

// startMultiTail runs one goroutine per pod, all under a single cancellable
// context stored in a.logsCancel (so closing the pane stops every stream). The
// client is pinned on the UI goroutine, as every action must.
func (a *App) startMultiTail(st *multiLogState, targets []Row, labels []string) {
	a.stopLogs()
	ctx, cancel := context.WithCancel(context.Background())
	a.logsCancel = cancel

	cl := a.client
	for i, t := range targets {
		go func(t Row, label string) {
			cont, err := cl.PodMainContainer(ctx, t.Namespace, t.Name)
			if err != nil {
				a.pushTailLines(ctx, st, label, []string{fmt.Sprintf("error: %v", err)}, true)
				return
			}
			stream, err := cl.StreamPodLogs(ctx, t.Namespace, t.Name, cont, true, false, multiTailLines)
			if err != nil {
				a.pushTailLines(ctx, st, label, []string{fmt.Sprintf("error: %v", err)}, true)
				return
			}
			defer stream.Close()

			// Line splitting happens here, on the streaming goroutine: partial is
			// goroutine-local, so only whole lines cross to the UI goroutine.
			var partial string
			buf := make([]byte, 8*1024)
			for {
				n, rerr := stream.Read(buf)
				if n > 0 {
					partial += string(buf[:n])
					var lines []string
					for {
						nl := strings.IndexByte(partial, '\n')
						if nl < 0 {
							break
						}
						lines = append(lines, strings.TrimRight(partial[:nl], "\r"))
						partial = partial[nl+1:]
					}
					if len(lines) > 0 {
						a.pushTailLines(ctx, st, label, lines, false)
					}
				}
				if rerr != nil {
					break // EOF (pod gone) or context cancellation
				}
			}
		}(t, labels[i])
	}
}

// pushTailLines appends lines from one pod to the shared buffer and repaints.
// The mutation runs on the UI goroutine; the ctx guard drops writes from streams
// that outlived their pane. QueueUpdateDraw blocking is also the backpressure
// that keeps a chatty pod from starving the others.
func (a *App) pushTailLines(ctx context.Context, st *multiLogState, pod string, lines []string, isErr bool) {
	a.tv.QueueUpdateDraw(func() {
		if ctx.Err() != nil {
			return
		}
		for _, l := range lines {
			st.lines = append(st.lines, mline{pod: pod, text: l, err: isErr})
		}
		if len(st.lines) > logMaxLines {
			st.lines = st.lines[len(st.lines)-logMaxLines:]
		}
		st.render()
	})
}

// showMultiLogGrep filters the tail buffer in place (no re-stream), matching
// against the pod label as well as the line, so "grep: api" narrows to one pod.
func (a *App) showMultiLogGrep(st *multiLogState, setTitle func()) {
	input := tview.NewInputField().
		SetLabel("grep: ").
		SetText(st.grep).
		SetFieldWidth(30)
	done := func() {
		a.pages.RemovePage("multigrep")
		a.tv.SetFocus(st.view)
	}
	input.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			st.grep = strings.TrimSpace(input.GetText())
			done()
			setTitle()
			st.render()
		case tcell.KeyEscape:
			done()
		}
	})
	input.SetBorder(true).SetTitle(" grep tail — pod or line (empty = clear) ")
	a.openModal("multigrep", input, 52, 3)
}

// closeMultiLogs stops every stream and removes the pane.
func (a *App) closeMultiLogs() {
	a.stopLogs()
	a.closeModal("multilog")
}

// tailLabels names each pod in the prefix column: bare name when every target
// shares a namespace, ns/name when the tail spans namespaces (all-namespaces
// mode), so identically named pods stay distinct.
func tailLabels(targets []Row) []string {
	multiNS := false
	for _, t := range targets {
		if t.Namespace != targets[0].Namespace {
			multiNS = true
			break
		}
	}
	out := make([]string, len(targets))
	for i, t := range targets {
		if multiNS && t.Namespace != "" {
			out[i] = t.Namespace + "/" + t.Name
		} else {
			out[i] = t.Name
		}
	}
	return out
}

// truncLabel shortens a long pod name to the prefix width, keeping the tail
// (the generated suffix is what distinguishes replicas).
func truncLabel(s string, w int) string {
	if len(s) <= w {
		return s
	}
	if w <= 1 {
		return s[:w]
	}
	return "…" + s[len(s)-(w-1):]
}
