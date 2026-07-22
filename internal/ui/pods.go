package ui

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// drawTable repaints the table for the current view, preserving the selected
// row. The view's StatusCol (if any) is color-coded.
func (a *App) drawTable() {
	row, _ := a.table.GetSelection()
	rows := a.filteredRows()
	drawRows(a.table, a.view(), rows, a.marked)

	// Keep the cursor in range after the row count changes.
	if max := len(rows); row > max {
		row = max
	}
	if row < 1 && len(rows) > 0 {
		row = 1
	}
	a.table.Select(row, 0)
}

// drawRows fills a table widget from display-ready rows. It takes the view and
// rows explicitly (rather than reading App state) so the same renderer paints
// the live pane and the parked tab shown in the other split pane.
func drawRows(tbl *tview.Table, view *viewDef, rows []Row, marked map[string]bool) {
	tbl.Clear()
	for c, title := range view.Columns {
		tbl.SetCell(0, c, tview.NewTableCell(title).
			SetTextColor(tcell.ColorYellow).
			SetAttributes(tcell.AttrBold).
			SetSelectable(false))
	}
	for r, row := range rows {
		isMarked := marked[rowKey(row)]
		for c, val := range row.Cells {
			// Escape so message text containing "[...]" is not parsed as a
			// tview colour tag; status colouring uses SetTextColor, not tags.
			cell := tview.NewTableCell(tview.Escape(val))
			if c == view.StatusCol {
				cell.SetTextColor(statusColor(val))
			}
			if isMarked {
				cell.SetBackgroundColor(markColor) // multi-select highlight
			}
			tbl.SetCell(r+1, c, cell)
		}
	}
}

// onTableKey handles global key bindings while the table is focused.
func (a *App) onTableKey(event *tcell.EventKey) *tcell.EventKey {
	// View switching: Tab / Shift-Tab cycle, digits jump directly.
	switch event.Key() {
	case tcell.KeyTab:
		a.cycleView(1)
		return nil
	case tcell.KeyBacktab:
		a.cycleView(-1)
		return nil
	case tcell.KeyEnter:
		switch {
		case a.view().Local:
			a.showForwardLog() // Port-Fwd view: Enter shows the forward's log
		case a.view().Pulse:
			a.jumpFromPulse() // Pulse: Enter opens the kind under the cursor
		default:
			a.showDetail()
		}
		return nil
	case tcell.KeyEscape:
		if a.filter != "" {
			a.clearFilter() // Esc clears an active filter; no-op otherwise
			return nil
		}
	}

	// Alt+1..9 jump straight to tab N (checked before the plain-digit view jump).
	if event.Modifiers()&tcell.ModAlt != 0 {
		if r := event.Rune(); r >= '1' && r <= '9' {
			a.gotoTab(int(r - '1'))
			return nil
		}
	}

	// Number keys 1..N jump straight to a view; 0 is the Pulse summary, which
	// sits past the numbered views.
	if r := event.Rune(); r >= '1' && r <= '9' {
		a.switchView(int(r - '1'))
		return nil
	}
	if event.Rune() == '0' {
		a.gotoPulse()
		return nil
	}

	caps := a.view().Caps
	switch event.Rune() {
	case 'q':
		if a.view().Local {
			a.backView() // in a hidden view (Port-Fwd), q returns to the previous view
		} else {
			a.tv.Stop()
		}
		return nil
	case ' ':
		a.toggleMark() // multi-select: mark/unmark the current row (Delete-capable views)
		return nil
	case 't':
		a.newTab() // open a new tab cloning the current view
		return nil
	case 'T':
		a.renameTab() // set a custom label for the active tab
		return nil
	case 'w':
		a.closeTab() // close the active tab (keeps at least one)
		return nil
	case '[':
		a.prevTab()
		return nil
	case ']':
		a.nextTab()
		return nil
	case '|':
		a.toggleSplit(splitVert) // two panes side by side (press again to unsplit)
		return nil
	case '-':
		a.toggleSplit(splitHoriz) // two panes stacked
		return nil
	case '\\':
		a.swapPane() // move focus (and the live session) to the other pane
		return nil
	case 'r':
		go a.refresh()
		return nil
	case 'n':
		a.showNamespacePicker()
		return nil
	case 'x':
		a.showContextPicker()
		return nil
	case '?':
		a.showHelp()
		return nil
	case ':':
		a.showCommandDialog() // command-jump to any resource by name/alias
		return nil
	case '/':
		a.showFilterDialog()
		return nil
	case '.':
		a.cycleSort()
		return nil
	case ',':
		a.toggleSortDir()
		return nil
	case 'F':
		a.gotoForwardsView() // global: jump to the Port-Fwd view
		return nil
	case 's':
		if caps.Scale {
			a.showScaleDialog()
		}
		return nil
	case 'l':
		if caps.Logs {
			a.showLogs()
		}
		return nil
	case 'L':
		if caps.Logs {
			a.showMultiLogs() // tail marked rows (or the whole filtered list) at once
		}
		return nil
	case 'e':
		if caps.Exec {
			a.execShell()
		}
		return nil
	case 'E':
		if caps.Edit {
			a.showEdit()
		}
		return nil
	case 'g':
		if caps.Graph {
			a.showGraph()
		}
		return nil
	case 'f':
		if caps.PortForward {
			a.showPortForwardDialog()
		}
		return nil
	case 'd':
		if a.view().Local {
			a.stopSelectedForward() // Port-Fwd view: d stops the forward
		} else if caps.Delete {
			a.confirmDelete()
		}
		return nil
	case 'R':
		if caps.Restart {
			a.confirmRestart()
		}
		return nil
	case 'u':
		if caps.Rollback {
			a.confirmRollback()
		}
		return nil
	case 'v':
		if caps.Reveal {
			a.showSecretReveal()
		}
		return nil
	case 'c':
		if caps.Cordon {
			a.confirmCordon()
		}
		return nil
	case 'D':
		if caps.Drain {
			a.confirmDrain()
		}
		return nil
	case 'a':
		if a.splash != nil {
			a.toggleSplash() // show/hide the corner animation
		}
		return nil
	case 'A':
		if a.sixelEnabled {
			a.showSixel()
		}
		return nil
	}
	return event
}

func statusColor(status string) tcell.Color {
	switch status {
	case "Running", "Succeeded", "Completed", "Bound", "Normal", "OK":
		return tcell.ColorGreen
	case "Pending", "ContainerCreating", "Terminating", "WARN":
		return tcell.ColorYellow
	case "":
		return tcell.ColorWhite
	default: // Failed, CrashLoopBackOff, Error, ImagePullBackOff, ...
		return tcell.ColorRed
	}
}

// boolStr renders a bool as "true"/"false" for table cells.
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// itoa is a tiny int-to-string without pulling in strconv at call sites.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
