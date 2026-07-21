package ui

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// portForward tracks one background port-forward. All fields are mutated only
// on the UI goroutine (via QueueUpdateDraw), so no locking is needed.
type portForward struct {
	id      int
	ns, pod string
	ports   []string
	stopCh  chan struct{}
	status  string // "starting", "forwarding", "closed", "error: ..."
	stopped bool
}

// showPortForwardDialog prompts for the port mapping to forward to the selected
// pod, then starts a background forward.
func (a *App) showPortForwardDialog() {
	row, ok := a.selectedRow()
	if !ok {
		return
	}
	ns, name := row.Namespace, row.Name
	isService := a.view().ID == "service"

	input := tview.NewInputField().
		SetLabel("Ports (local:remote): ").
		SetText("8080:80").
		SetFieldWidth(20)

	form := tview.NewForm().AddFormItem(input)
	form.AddButton("Forward", func() {
		ports := strings.Fields(input.GetText())
		if len(ports) == 0 {
			a.showMessage("pf", "enter at least one local:remote mapping")
			return
		}
		a.closeModal("pf-dialog")
		if isService {
			// A Service has no pod of its own; resolve it to a Ready backing pod
			// and forward to that, transparently.
			cl := a.client
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				pod, err := cl.ServiceForwardTarget(ctx, ns, name)
				a.tv.QueueUpdateDraw(func() {
					if err != nil {
						a.showMessage("pf", fmt.Sprintf("error: %v", err))
						return
					}
					a.startPortForward(ns, pod, ports)
				})
			}()
			return
		}
		a.startPortForward(ns, name, ports)
	})
	form.AddButton("Cancel", func() { a.closeModal("pf-dialog") })
	form.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape {
			a.closeModal("pf-dialog")
			return nil
		}
		return e
	})
	form.SetBorder(true).SetTitle(fmt.Sprintf(" port-forward %s/%s ", ns, name))

	a.openModal("pf-dialog", form, 50, 7)
}

// startPortForward begins a forward that keeps running in the background after
// the dialog closes; manage/stop it later via the port-forward manager (F).
func (a *App) startPortForward(ns, name string, ports []string) {
	pf := &portForward{
		id:     a.nextFwdID,
		ns:     ns,
		pod:    name,
		ports:  ports,
		stopCh: make(chan struct{}),
		status: "starting",
	}
	a.nextFwdID++
	a.forwards = append(a.forwards, pf)
	a.drawHeader()

	cl := a.client // pin the cluster this forward runs against, in case the context switches
	readyCh := make(chan struct{})
	go func() {
		<-readyCh // closed by portforward once established
		a.tv.QueueUpdateDraw(func() {
			if !pf.stopped {
				pf.status = "forwarding"
				a.redrawForwards()
			}
		})
	}()
	go func() {
		err := cl.PortForward(ns, name, ports, io.Discard, io.Discard, pf.stopCh, readyCh)
		a.tv.QueueUpdateDraw(func() {
			if pf.stopped {
				return // torn down on purpose
			}
			if err != nil {
				pf.status = "error: " + err.Error()
			} else {
				pf.status = "closed" // ended without us stopping it
			}
			a.redrawForwards()
		})
	}()

	a.showMessage("pf", fmt.Sprintf("port-forward started (background):\n%s\n\nPress F to open the Port-Fwd view.",
		forwardLines(ports)))
}

// redrawForwards repaints the table when the Port-Fwd view is active, so status
// changes show without waiting for the next refresh tick. Also keeps the header
// count current.
func (a *App) redrawForwards() {
	a.drawHeader()
	if a.view().Local {
		a.rows = a.forwardRows()
		a.drawTable()
	}
}

// forwardRows renders the active port-forwards as table rows for the Port-Fwd
// view. The ID column keys back to the *portForward across filter/sort.
func (a *App) forwardRows() []Row {
	rows := make([]Row, len(a.forwards))
	for i, pf := range a.forwards {
		rows[i] = Row{
			Namespace: pf.ns,
			Name:      pf.pod,
			Cells: []string{itoa(pf.id), pf.ns, pf.pod,
				strings.Join(pf.ports, " "), pf.status},
		}
	}
	return rows
}

// gotoForwardsView switches to the Port-Fwd view, remembering the current view
// so `<` can return to it.
func (a *App) gotoForwardsView() {
	for i, v := range resourceViews {
		if v.Local {
			if !a.view().Local {
				a.prevViewIdx = a.viewIdx // remember where we came from
			}
			a.switchView(i)
			return
		}
	}
}

// backView returns from a hidden (Local) view to the previously active one.
func (a *App) backView() {
	if a.view().Local {
		a.switchView(a.prevViewIdx)
	}
}

// stopSelectedForward tears down the forward under the cursor (Port-Fwd view).
func (a *App) stopSelectedForward() {
	row, ok := a.selectedRow()
	if !ok || len(row.Cells) == 0 {
		return
	}
	id, err := strconv.Atoi(row.Cells[0])
	if err != nil {
		return
	}
	for _, pf := range a.forwards {
		if pf.id == id {
			a.stopForward(pf)
			break
		}
	}
	a.rows = a.forwardRows()
	a.drawHeader()
	a.drawTable()
}

// stopForward tears a forward down and drops it from the registry.
func (a *App) stopForward(pf *portForward) {
	if pf.stopped {
		return
	}
	pf.stopped = true
	close(pf.stopCh)
	for i, f := range a.forwards {
		if f == pf {
			a.forwards = append(a.forwards[:i], a.forwards[i+1:]...)
			break
		}
	}
}

// forwardLines formats each mapping as "localhost:LOCAL -> REMOTE".
func forwardLines(ports []string) string {
	lines := make([]string, 0, len(ports))
	for _, p := range ports {
		local, remote, found := strings.Cut(p, ":")
		if !found {
			lines = append(lines, p)
			continue
		}
		lines = append(lines, fmt.Sprintf("localhost:%s -> %s", local, remote))
	}
	return strings.Join(lines, "\n")
}
