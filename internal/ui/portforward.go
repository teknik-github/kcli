package ui

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/teknik-github/kcli/internal/k8s"
)

// pfLog is a bounded, line-oriented, concurrency-safe sink for a port-forward's
// out/errOut streams (the "Forwarding from …" notices and per-connection
// errors). The forwarding goroutine writes; the UI reads via text().
type pfLog struct {
	mu    sync.Mutex
	lines []string
	buf   []byte // accumulates bytes until a newline completes a line
}

const pfLogMaxLines = 500

func (l *pfLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(l.buf[:i]), "\r")
		l.lines = append(l.lines, time.Now().Format("15:04:05")+"  "+line)
		l.buf = append([]byte(nil), l.buf[i+1:]...)
	}
	if len(l.lines) > pfLogMaxLines {
		l.lines = l.lines[len(l.lines)-pfLogMaxLines:]
	}
	return len(p), nil
}

// text returns the accumulated log, including any trailing partial line.
func (l *pfLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := strings.Join(l.lines, "\n")
	if len(l.buf) > 0 {
		if out != "" {
			out += "\n"
		}
		out += string(l.buf)
	}
	return out
}

// portForward tracks one background port-forward. All fields are mutated only
// on the UI goroutine (via QueueUpdateDraw), so no locking is needed.
type portForward struct {
	id      int
	ns, pod string
	ports   []string
	stopCh  chan struct{}
	status  string // "starting", "forwarding", "closed", "error: ..."
	stopped bool
	log     *pfLog // captured out/errOut, shown in the log view
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
				pod, mapped, err := cl.ServiceForward(ctx, ns, name, ports)
				a.tv.QueueUpdateDraw(func() {
					if err != nil {
						a.showMessage("pf", fmt.Sprintf("error: %v", err))
						return
					}
					a.startPortForward(ns, pod, mapped)
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
		log:    &pfLog{},
	}
	a.nextFwdID++
	a.forwards = append(a.forwards, pf)
	// redrawForwards (not just drawHeader) so the row shows immediately when the
	// forward is added while the Port-Fwd view is already active — e.g. after an
	// async Service→pod resolve that finished while the user was on that view.
	a.redrawForwards()

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
		err := cl.PortForward(ns, name, ports, pf.log, pf.log, pf.stopCh, readyCh)
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
	pf := a.selectedForward()
	if pf == nil {
		return
	}
	a.stopForward(pf)
	a.rows = a.forwardRows()
	a.drawHeader()
	a.drawTable()
}

// selectedForward returns the *portForward for the row under the cursor (keyed
// by the ID column, which survives filter/sort), or nil.
func (a *App) selectedForward() *portForward {
	row, ok := a.selectedRow()
	if !ok || len(row.Cells) == 0 {
		return nil
	}
	id, err := strconv.Atoi(row.Cells[0])
	if err != nil {
		return nil
	}
	for _, pf := range a.forwards {
		if pf.id == id {
			return pf
		}
	}
	return nil
}

// showForwardLog opens a live view of the selected forward's captured output —
// the "Forwarding from …" notices and any per-connection errors — so its
// progress is visible. It refreshes on a ticker until closed (q/esc).
func (a *App) showForwardLog() {
	pf := a.selectedForward()
	if pf == nil {
		return
	}
	view := tview.NewTextView().SetScrollable(true).SetWrap(true)
	view.SetBorder(true).SetTitle(fmt.Sprintf(" forward %s/%s [%s]  q/esc close ",
		pf.ns, pf.pod, strings.Join(pf.ports, " ")))
	render := func() {
		body := pf.log.text()
		if body == "" {
			body = "(no output yet — the forwarder logs here once traffic flows)"
		}
		view.SetText(fmt.Sprintf("status: %s\n\n%s", pf.status, body))
		view.ScrollToEnd()
	}
	render()
	view.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape || e.Rune() == 'q' {
			a.stopPFLog()
			a.closeModal("pflog")
			return nil
		}
		return e
	})
	a.openModalFull("pflog", view)

	stop := make(chan struct{})
	a.pflogStop = stop
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				a.tv.QueueUpdateDraw(render)
			}
		}
	}()
}

// stopPFLog halts the port-forward log view's refresh ticker, if running.
func (a *App) stopPFLog() {
	if a.pflogStop != nil {
		close(a.pflogStop)
		a.pflogStop = nil
	}
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

// forwardLines formats each mapping as "<bind>:LOCAL -> REMOTE", reflecting the
// configured bind address (KCLI_PF_ADDRESS, default 0.0.0.0).
func forwardLines(ports []string) string {
	addr := k8s.ForwardBindAddresses()[0]
	lines := make([]string, 0, len(ports))
	for _, p := range ports {
		local, remote, found := strings.Cut(p, ":")
		if !found {
			lines = append(lines, p)
			continue
		}
		lines = append(lines, fmt.Sprintf("%s:%s -> %s", addr, local, remote))
	}
	return strings.Join(lines, "\n")
}
