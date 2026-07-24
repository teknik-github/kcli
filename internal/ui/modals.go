package ui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// showNamespacePicker opens a list of namespaces (plus "<all>") to switch the
// active namespace filter.
func (a *App) showNamespacePicker() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	names, err := a.client.Namespaces(ctx)
	if err != nil {
		a.showMessage("namespaces", fmt.Sprintf("error: %v", err))
		return
	}

	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(" namespace ")
	list.AddItem("<all namespaces>", "", 0, func() {
		a.setNamespace("")
	})
	for _, n := range names {
		name := n // capture
		list.AddItem(name, "", 0, func() {
			a.setNamespace(name)
		})
	}
	list.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape {
			a.closeModal("namespace")
			return nil
		}
		return e
	})

	a.openModal("namespace", list, 40, 20)
}

func (a *App) setNamespace(ns string) {
	a.namespace = ns
	a.clearMarks() // row identities change with the namespace
	a.closeModal("namespace")
	a.drawTabChrome() // the tab label carries the namespace
	go a.refresh()
}

// showContextPicker lists the kubeconfig contexts and switches to the chosen
// one. The active context is marked with "*".
func (a *App) showContextPicker() {
	names, err := a.client.Contexts()
	if err != nil {
		a.showMessage("context", fmt.Sprintf("error: %v", err))
		return
	}
	if len(names) == 0 {
		a.showMessage("context", "no contexts found in kubeconfig")
		return
	}

	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(" context ")
	for _, n := range names {
		name := n // capture
		label := name
		if name == a.client.Context {
			label = "* " + name // mark the active context
		}
		list.AddItem(label, "", 0, func() {
			a.switchContext(name)
		})
	}
	list.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape {
			a.closeModal("context")
			return nil
		}
		return e
	})
	a.openModal("context", list, 60, 20)
}

// switchContext rebuilds the client for the chosen context and reloads.
// Cluster-specific state (namespace, filter, sort) is reset, and the client
// generation is bumped so any in-flight refresh from the old cluster is dropped.
func (a *App) switchContext(name string) {
	if name == a.client.Context {
		a.closeModal("context")
		return
	}
	nc, err := a.client.WithContext(name)
	if err != nil {
		a.closeModal("context")
		a.showMessage("context", fmt.Sprintf("error: %v", err))
		return
	}
	a.closePaneOverlay() // a graph samples the old cluster's pod; it can't follow
	a.client.Stop()      // tear down the old cluster's informers/watches
	nc.SetOnChange(a.onChange)
	a.client = nc
	a.clientGen++
	a.namespace = ""
	a.filter = ""
	a.sortCol = -1
	a.sortDesc = false
	a.clearMarks()
	a.closeModal("context")
	a.table.Clear()
	a.drawHeader()
	a.drawTabs()
	go a.refresh()
}

// pickContainer resolves which container an action targets: it runs `then`
// directly for single-container pods, or pops a picker for multi-container
// ones. Container names are fetched off the UI goroutine.
func (a *App) pickContainer(namespace, name string, then func(container string)) {
	cl := a.client
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conts, err := cl.PodContainers(ctx, namespace, name)
		a.tv.QueueUpdateDraw(func() {
			switch {
			case err != nil:
				a.showMessage("container", fmt.Sprintf("error: %v", err))
			case len(conts) == 0:
				a.showMessage("container", "pod has no containers")
			case len(conts) == 1:
				then(conts[0])
			default:
				a.showContainerList(name, conts, then)
			}
		})
	}()
}

// showContainerList presents the container names to choose from.
func (a *App) showContainerList(pod string, conts []string, then func(container string)) {
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" container: %s ", pod))
	for _, c := range conts {
		name := c // capture
		list.AddItem(name, "", 0, func() {
			a.closeModal("container")
			then(name)
		})
	}
	list.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape {
			a.closeModal("container")
			return nil
		}
		return e
	})
	a.openModal("container", list, 44, len(conts)+2)
}

// logMaxLines caps how many log lines the follow buffer retains.
const logMaxLines = 5000

// showLogs streams the selected pod's container logs into a scrollable pane,
// prompting for a container first on multi-container pods.
func (a *App) showLogs() {
	pod, ok := a.selectedRow()
	if !ok {
		return
	}
	a.pickContainer(pod.Namespace, pod.Name, func(container string) {
		a.openLogs(pod.Namespace, pod.Name, container)
	})
}

// logState holds a log pane's rolling buffer and its active grep. All fields are
// touched only on the UI goroutine (inside QueueUpdateDraw / key handlers), so
// the grep filter can re-render the same buffer without re-streaming.
type logState struct {
	view    *tview.TextView
	lines   []string // complete lines, capped at logMaxLines
	partial string   // trailing fragment not yet terminated by a newline
	grep    string   // case-insensitive line filter; "" shows everything
	any     bool     // whether any bytes have arrived
}

// render repaints the pane from the buffer, applying the grep filter.
func (st *logState) render() {
	shown := st.lines
	if st.grep != "" {
		g := strings.ToLower(st.grep)
		shown = shown[:0:0]
		for _, ln := range st.lines {
			if strings.Contains(strings.ToLower(ln), g) {
				shown = append(shown, ln)
			}
		}
	}
	text := strings.Join(shown, "\n")
	if st.grep == "" && st.partial != "" { // the unterminated tail only shows unfiltered
		if text != "" {
			text += "\n"
		}
		text += st.partial
	}
	if !st.any {
		st.view.SetText("(no logs)")
		return
	}
	st.view.SetText(tview.Escape(text))
	st.view.ScrollToEnd()
}

// openLogs opens the log pane and starts following the container's logs. Inside
// the pane 'p' toggles live/previous, '/' greps the buffer, q/esc close.
func (a *App) openLogs(namespace, name, container string) {
	view := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(false)
	view.SetBorder(true)

	st := &logState{view: view}
	previous := false
	setTitle := func() {
		mode := "follow"
		if previous {
			mode = "previous"
		}
		grep := ""
		if st.grep != "" {
			grep = fmt.Sprintf("  grep:%q", st.grep)
		}
		view.SetTitle(fmt.Sprintf(" logs: %s/%s [%s] (%s)%s  p:prev  /:grep  q:close ",
			namespace, name, container, mode, grep))
	}
	setTitle()

	view.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		switch {
		case e.Key() == tcell.KeyEscape || e.Rune() == 'q':
			a.closeLogs()
			return nil
		case e.Rune() == 'p':
			previous = !previous
			setTitle()
			a.streamLogs(st, namespace, name, container, previous)
			return nil
		case e.Rune() == '/':
			a.showLogGrep(st, setTitle)
			return nil
		}
		return e
	})

	a.openModalFull("logs", view)
	a.streamLogs(st, namespace, name, container, previous)
}

// showLogGrep prompts for a substring to filter the log buffer by, re-rendering
// in place (no re-stream). An empty value clears the filter.
func (a *App) showLogGrep(st *logState, setTitle func()) {
	input := tview.NewInputField().
		SetLabel("grep: ").
		SetText(st.grep).
		SetFieldWidth(30)
	done := func() {
		a.pages.RemovePage("loggrep")
		a.tv.SetFocus(st.view) // back to the log pane, not the table
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
	input.SetBorder(true).SetTitle(" grep logs (empty = clear) ")
	a.openModal("loggrep", input, 46, 3)
}

// streamLogs (re)starts the log stream into st, cancelling any prior stream and
// resetting the buffer. Live logs follow until the pane closes; previous-container
// logs are a one-shot snapshot. The ctx.Err() guards drop late writes from a
// stream that has been superseded. All buffer mutation happens on the UI
// goroutine so a concurrent grep re-render stays consistent.
func (a *App) streamLogs(st *logState, namespace, name, container string, previous bool) {
	a.stopLogs()
	ctx, cancel := context.WithCancel(context.Background())
	a.logsCancel = cancel

	st.lines = nil
	st.partial = ""
	st.any = false
	st.view.Clear()
	fmt.Fprint(st.view, "[gray]streaming…[-]")

	cl := a.client
	go func() {
		stream, err := cl.StreamPodLogs(ctx, namespace, name, container, !previous, previous, 500)
		if err != nil {
			a.tv.QueueUpdateDraw(func() {
				if ctx.Err() != nil {
					return
				}
				st.view.SetText(fmt.Sprintf("[red]error: %v[-]", err))
			})
			return
		}
		defer stream.Close()

		buf := make([]byte, 8*1024)
		for {
			n, rerr := stream.Read(buf)
			if n > 0 {
				chunk := string(buf[:n])
				a.tv.QueueUpdateDraw(func() {
					if ctx.Err() != nil {
						return
					}
					st.any = true
					st.partial += chunk
					for {
						nl := strings.IndexByte(st.partial, '\n')
						if nl < 0 {
							break
						}
						st.lines = append(st.lines, st.partial[:nl])
						st.partial = st.partial[nl+1:]
					}
					if len(st.lines) > logMaxLines {
						st.lines = st.lines[len(st.lines)-logMaxLines:]
					}
					st.render()
				})
			}
			if rerr != nil {
				break // io.EOF (snapshot done) or context cancellation
			}
		}
		a.tv.QueueUpdateDraw(func() {
			if ctx.Err() != nil {
				return
			}
			if !st.any {
				st.view.SetText("(no logs)")
			}
		})
	}()
}

// stopLogs cancels the active log stream, if any.
func (a *App) stopLogs() {
	if a.logsCancel != nil {
		a.logsCancel()
		a.logsCancel = nil
	}
}

// closeLogs stops streaming and removes the log pane.
func (a *App) closeLogs() {
	a.stopLogs()
	a.closeModal("logs")
}

// showDetail opens a scrollable pane with the YAML + events of the selected
// resource, akin to `kubectl describe`.
func (a *App) showDetail() {
	kind, ns, name, ok := a.selectedName()
	if !ok {
		return
	}

	dynamic := a.view().Dynamic
	gvr, nsd := a.dynGVR, a.dynNamespaced
	titleKind := kind
	if dynamic {
		titleKind = a.view().Title // "dynamic" is the internal ID; show the real Kind
	}

	view := tview.NewTextView().
		SetDynamicColors(false).
		SetScrollable(true).
		SetWrap(false)
	view.SetBorder(true).SetTitle(fmt.Sprintf(" %s: %s/%s ", titleKind, ns, name))
	view.SetText("loading…")
	view.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape || e.Rune() == 'q' {
			a.closeModal("detail")
			return nil
		}
		return e
	})
	a.openModalFull("detail", view)

	cl := a.client
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var text string
		var err error
		if dynamic {
			text, err = cl.DescribeDynamic(ctx, gvr, nsd, ns, name)
		} else {
			text, err = cl.Describe(ctx, kind, ns, name)
		}
		a.tv.QueueUpdateDraw(func() {
			if err != nil {
				view.SetText(fmt.Sprintf("error: %v", err))
				return
			}
			view.SetText(text)
			view.ScrollToBeginning()
		})
	}()
}

// showScaleDialog prompts for a new replica count for the selected deployment.
// The current replicas prefill is parsed from the READY column ("ready/desired").
func (a *App) showScaleDialog() {
	row, ok := a.selectedRow()
	if !ok {
		return
	}
	kind := a.view().ID
	ns, name := row.Namespace, row.Name
	desired := "0"
	if len(row.Cells) > 2 {
		if _, d, found := strings.Cut(row.Cells[2], "/"); found {
			desired = d
		}
	}

	input := tview.NewInputField().
		SetLabel("Replicas: ").
		SetText(desired).
		SetFieldWidth(6).
		SetAcceptanceFunc(tview.InputFieldInteger) // digits only

	form := tview.NewForm().AddFormItem(input)
	form.AddButton("Scale", func() {
		n, err := strconv.Atoi(input.GetText())
		if err != nil || n < 0 {
			a.showMessage("scale", "invalid replica count")
			return
		}
		a.closeModal("scale")
		cl := a.client
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := cl.Scale(ctx, kind, ns, name, int32(n)); err != nil {
				a.tv.QueueUpdateDraw(func() {
					a.showMessage("scale", fmt.Sprintf("error: %v", err))
				})
				return
			}
			a.refresh()
		}()
	})
	form.AddButton("Cancel", func() { a.closeModal("scale") })
	form.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape {
			a.closeModal("scale")
			return nil
		}
		return e
	})
	form.SetBorder(true).SetTitle(fmt.Sprintf(" scale %s/%s ", ns, name))

	a.openModal("scale", form, 44, 7)
}

// confirmDelete asks before deleting the resource under the cursor. Only views
// whose Caps.Delete is set reach here.
func (a *App) confirmDelete() {
	if len(a.marked) > 0 {
		a.confirmBulkDelete() // multi-select: delete every marked row instead
		return
	}
	kind, ns, name, ok := a.selectedName()
	if !ok {
		return
	}

	modal := tview.NewModal().
		SetText(fmt.Sprintf("Delete %s %s/%s?", kind, ns, name)).
		AddButtons([]string{"Cancel", "Delete"}).
		SetDoneFunc(func(_ int, label string) {
			a.closeModal("confirm")
			if label != "Delete" {
				return
			}
			cl := a.client
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := cl.Delete(ctx, kind, ns, name); err != nil {
					a.tv.QueueUpdateDraw(func() {
						a.showMessage("delete", fmt.Sprintf("error: %v", err))
					})
					return
				}
				a.refresh()
			}()
		})
	a.pages.AddPage("confirm", modal, true, true)
	a.tv.SetFocus(modal)
}

// confirm shows a Cancel/<okLabel> modal, running onYes only when confirmed.
func (a *App) confirm(page, message, okLabel string, onYes func()) {
	modal := tview.NewModal().
		SetText(message).
		AddButtons([]string{"Cancel", okLabel}).
		SetDoneFunc(func(_ int, label string) {
			a.closeModal(page)
			if label == okLabel {
				onYes()
			}
		})
	a.pages.AddPage(page, modal, true, true)
	a.tv.SetFocus(modal)
}

// confirmRestart asks before triggering a rollout restart of the selected
// workload. Only views whose Caps.Restart is set reach here.
func (a *App) confirmRestart() {
	kind, ns, name, ok := a.selectedName()
	if !ok {
		return
	}
	cl := a.client
	a.confirm("restart", fmt.Sprintf("Rollout restart %s %s/%s?", kind, ns, name), "Restart", func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := cl.RolloutRestart(ctx, kind, ns, name); err != nil {
				a.tv.QueueUpdateDraw(func() {
					a.showMessage("restart", fmt.Sprintf("error: %v", err))
				})
				return
			}
			a.refresh()
		}()
	})
}

// confirmCordon toggles the selected node's schedulability. The current state
// is read from the STATUS cell (col 1), which carries ",SchedulingDisabled"
// when cordoned. Only the Nodes view (Caps.Cordon) reaches here.
func (a *App) confirmCordon() {
	row, ok := a.selectedRow()
	if !ok {
		return
	}
	name := row.Name
	cordoned := strings.Contains(cellAt(row, 1), "SchedulingDisabled")
	action := "Cordon"
	if cordoned {
		action = "Uncordon"
	}
	cl := a.client
	a.confirm("cordon", fmt.Sprintf("%s node %s?", action, name), action, func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := cl.CordonNode(ctx, name, !cordoned); err != nil {
				a.tv.QueueUpdateDraw(func() {
					a.showMessage("cordon", fmt.Sprintf("error: %v", err))
				})
				return
			}
			a.refresh()
		}()
	})
}

// confirmRollback rolls the selected workload back to its previous revision
// (kubectl rollout undo). Only views whose Caps.Rollback is set reach here.
func (a *App) confirmRollback() {
	kind, ns, name, ok := a.selectedName()
	if !ok {
		return
	}
	cl := a.client
	a.confirm("rollback", fmt.Sprintf("Roll back %s %s/%s to the previous revision?", kind, ns, name), "Rollback", func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			msg, err := cl.RolloutUndo(ctx, kind, ns, name)
			a.tv.QueueUpdateDraw(func() {
				if err != nil {
					a.showMessage("rollback", fmt.Sprintf("error: %v", err))
				} else {
					a.showMessage("rollback", msg)
				}
			})
			a.refresh()
		}()
	})
}

// showSecretReveal confirms, then decodes and displays the selected secret's
// values in plain text. Only the Secrets view (Caps.Reveal) reaches here.
func (a *App) showSecretReveal() {
	kind, ns, name, ok := a.selectedName()
	if !ok || kind != "secret" {
		return
	}
	cl := a.client
	a.confirm("reveal", fmt.Sprintf("Reveal secret %s/%s values in plain text?", ns, name), "Reveal", func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			data, err := cl.SecretData(ctx, ns, name)
			a.tv.QueueUpdateDraw(func() {
				if err != nil {
					a.showMessage("reveal", fmt.Sprintf("error: %v", err))
					return
				}
				a.showSecretValues(ns, name, data)
			})
		}()
	})
}

// showSecretValues renders decoded secret keys/values in a scrollable pane.
func (a *App) showSecretValues(ns, name string, data map[string]string) {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	fmt.Fprintf(&b, "secret %s/%s — %d key(s)\n\n", ns, name, len(keys))
	for _, k := range keys {
		fmt.Fprintf(&b, "# %s\n%s\n\n", k, data[k])
	}

	view := tview.NewTextView().SetScrollable(true).SetWrap(true)
	view.SetBorder(true).SetTitle(fmt.Sprintf(" secret values: %s/%s  (q/esc close) ", ns, name))
	view.SetText(b.String())
	view.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape || e.Rune() == 'q' {
			a.closeModal("secretval")
			return nil
		}
		return e
	})
	a.openModalFull("secretval", view)
}

// confirmDrain cordons the selected node and evicts its evictable pods. Only
// the Nodes view (Caps.Drain) reaches here.
func (a *App) confirmDrain() {
	row, ok := a.selectedRow()
	if !ok {
		return
	}
	name := row.Name
	cl := a.client
	a.confirm("drain", fmt.Sprintf("Drain node %s?\n(cordon + evict all evictable pods)", name), "Drain", func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			n, err := cl.DrainNode(ctx, name)
			a.tv.QueueUpdateDraw(func() {
				if err != nil {
					a.showMessage("drain", fmt.Sprintf("drain %s: evicted %d, error: %v", name, n, err))
				} else {
					a.showMessage("drain", fmt.Sprintf("drained %s: evicted %d pod(s)", name, n))
				}
			})
			a.refresh()
		}()
	})
}

// showFilterDialog prompts for a name/namespace substring filter. Submitting an
// empty value clears the filter.
func (a *App) showFilterDialog() {
	input := tview.NewInputField().
		SetLabel("Filter: ").
		SetText(a.filter).
		SetFieldWidth(30)

	apply := func() {
		a.filter = strings.TrimSpace(input.GetText())
		a.closeModal("filter")
		a.table.Select(1, 0) // reset cursor; the row set changed
		a.drawHeader()
		a.drawTable()
	}

	input.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			apply()
		case tcell.KeyEscape:
			a.closeModal("filter")
		}
	})
	input.SetBorder(true).SetTitle(" filter (empty = clear) ")

	a.openModal("filter", input, 46, 3)
}

// showCommandDialog opens the ":" resource-jump prompt: type a resource name or
// alias (e.g. "svc", "cj", "ev", "pf") and Enter jumps to that view. This is the
// way to reach views past the 1..9 number keys. Esc cancels.
func (a *App) showCommandDialog() {
	input := tview.NewInputField().
		SetLabel(":").
		SetFieldWidth(24)
	input.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			q := strings.TrimSpace(input.GetText())
			a.closeModal("cmd")
			switch verb, rest, _ := strings.Cut(q, " "); {
			case q == "":
			case strings.EqualFold(verb, "ws"), strings.EqualFold(verb, "workspace"):
				a.workspaceCommand(rest) // ":ws save|load|rm|list [name]"
			case strings.EqualFold(q, "update"), strings.EqualFold(q, "upgrade"):
				a.updateCommand() // self-update via `go install …@latest`
			default:
				a.jumpToView(q)
			}
		case tcell.KeyEscape:
			a.closeModal("cmd")
		}
	})
	input.SetBorder(true).SetTitle(" :resource  (po svc deploy cj cm sec ev pf · ws save|load · update) ")
	a.openModal("cmd", input, 52, 3)
}

// showMessage displays a dismissable one-button modal.
func (a *App) showMessage(page, text string) {
	modal := tview.NewModal().
		SetText(text).
		AddButtons([]string{"OK"}).
		SetDoneFunc(func(int, string) { a.closeModal(page + "-msg") })
	a.pages.AddPage(page+"-msg", modal, true, true)
	a.tv.SetFocus(modal)
}

// openModal centers a primitive of a fixed size over the main view.
func (a *App) openModal(name string, p tview.Primitive, width, height int) {
	wrap := tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(p, height, 1, true).
			AddItem(nil, 0, 1, false), width, 1, true).
		AddItem(nil, 0, 1, false)
	a.pages.AddPage(name, wrap, true, true)
	a.tv.SetFocus(p)
}

// openModalFull overlays a primitive that fills the screen (minus a margin).
func (a *App) openModalFull(name string, p tview.Primitive) {
	a.pages.AddPage(name, p, true, true)
	a.tv.SetFocus(p)
}

// closeModal removes an overlay page and returns focus to the pod table.
func (a *App) closeModal(name string) {
	a.pages.RemovePage(name)
	a.tv.SetFocus(a.table)
}
