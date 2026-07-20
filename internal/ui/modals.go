package ui

import (
	"context"
	"fmt"
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
	a.closeModal("namespace")
	go a.refresh()
}

// pickContainer resolves which container an action targets: it runs `then`
// directly for single-container pods, or pops a picker for multi-container
// ones. Container names are fetched off the UI goroutine.
func (a *App) pickContainer(namespace, name string, then func(container string)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conts, err := a.client.PodContainers(ctx, namespace, name)
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

// openLogs opens the log pane and starts following the container's logs. Inside
// the pane 'p' toggles between the live (follow) stream and the previous
// (crashed) container's logs; q/esc close.
func (a *App) openLogs(namespace, name, container string) {
	view := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(false)
	view.SetBorder(true)

	previous := false
	setTitle := func() {
		mode := "follow"
		if previous {
			mode = "previous"
		}
		view.SetTitle(fmt.Sprintf(" logs: %s/%s [%s] (%s)  p:toggle-prev  q:close ",
			namespace, name, container, mode))
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
			a.streamLogs(view, namespace, name, container, previous)
			return nil
		}
		return e
	})

	a.openModalFull("logs", view)
	a.streamLogs(view, namespace, name, container, previous)
}

// streamLogs (re)starts the log stream into view, cancelling any prior stream.
// Live logs follow until the pane closes; previous-container logs are a
// one-shot snapshot (the container is gone, so there is nothing to follow). The
// ctx.Err() guards drop late writes from a stream that has been superseded.
func (a *App) streamLogs(view *tview.TextView, namespace, name, container string, previous bool) {
	a.stopLogs()
	ctx, cancel := context.WithCancel(context.Background())
	a.logsCancel = cancel

	view.Clear()
	fmt.Fprint(view, "[gray]streaming…[-]")

	go func() {
		stream, err := a.client.StreamPodLogs(ctx, namespace, name, container, !previous, previous, 500)
		if err != nil {
			a.tv.QueueUpdateDraw(func() {
				if ctx.Err() != nil {
					return
				}
				view.SetText(fmt.Sprintf("[red]error: %v[-]", err))
			})
			return
		}
		defer stream.Close()

		// Keep only the last logMaxLines so a long follow session cannot grow the
		// buffer without bound. lines holds complete lines; partial is the
		// trailing fragment not yet terminated by a newline.
		var lines []string
		var partial string
		any := false
		buf := make([]byte, 8*1024)
		for {
			n, rerr := stream.Read(buf)
			if n > 0 {
				any = true
				partial += string(buf[:n])
				for {
					nl := strings.IndexByte(partial, '\n')
					if nl < 0 {
						break
					}
					lines = append(lines, partial[:nl])
					partial = partial[nl+1:]
				}
				if len(lines) > logMaxLines {
					lines = lines[len(lines)-logMaxLines:]
				}
				text := strings.Join(lines, "\n")
				if partial != "" {
					if text != "" {
						text += "\n"
					}
					text += partial
				}
				display := tview.Escape(text)
				a.tv.QueueUpdateDraw(func() {
					if ctx.Err() != nil {
						return
					}
					view.SetText(display) // replaces the "streaming…" placeholder
					view.ScrollToEnd()
				})
			}
			if rerr != nil {
				break // io.EOF (snapshot done) or context cancellation
			}
		}
		if !any {
			a.tv.QueueUpdateDraw(func() {
				if ctx.Err() != nil {
					return
				}
				view.SetText("(no logs)")
			})
		}
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

	view := tview.NewTextView().
		SetDynamicColors(false).
		SetScrollable(true).
		SetWrap(false)
	view.SetBorder(true).SetTitle(fmt.Sprintf(" %s: %s/%s ", kind, ns, name))
	view.SetText("loading…")
	view.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape || e.Rune() == 'q' {
			a.closeModal("detail")
			return nil
		}
		return e
	})
	a.openModalFull("detail", view)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		text, err := a.client.Describe(ctx, kind, ns, name)
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
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := a.client.Scale(ctx, kind, ns, name, int32(n)); err != nil {
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
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := a.client.Delete(ctx, kind, ns, name); err != nil {
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
	a.confirm("restart", fmt.Sprintf("Rollout restart %s %s/%s?", kind, ns, name), "Restart", func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := a.client.RolloutRestart(ctx, kind, ns, name); err != nil {
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
	a.confirm("cordon", fmt.Sprintf("%s node %s?", action, name), action, func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := a.client.CordonNode(ctx, name, !cordoned); err != nil {
				a.tv.QueueUpdateDraw(func() {
					a.showMessage("cordon", fmt.Sprintf("error: %v", err))
				})
				return
			}
			a.refresh()
		}()
	})
}

// confirmDrain cordons the selected node and evicts its evictable pods. Only
// the Nodes view (Caps.Drain) reaches here.
func (a *App) confirmDrain() {
	row, ok := a.selectedRow()
	if !ok {
		return
	}
	name := row.Name
	a.confirm("drain", fmt.Sprintf("Drain node %s?\n(cordon + evict all evictable pods)", name), "Drain", func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			n, err := a.client.DrainNode(ctx, name)
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
