package ui

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// drawTable repaints the table for the current view, preserving the selected
// row. The view's StatusCol (if any) is color-coded.
func (a *App) drawTable() {
	row, _ := a.table.GetSelection()
	a.table.Clear()

	view := a.view()
	for c, title := range view.Columns {
		a.table.SetCell(0, c, tview.NewTableCell(title).
			SetTextColor(tcell.ColorYellow).
			SetAttributes(tcell.AttrBold).
			SetSelectable(false))
	}

	rows := a.filteredRows()
	for r, row := range rows {
		marked := a.marked[rowKey(row)]
		for c, val := range row.Cells {
			// Escape so message text containing "[...]" is not parsed as a
			// tview colour tag; status colouring uses SetTextColor, not tags.
			cell := tview.NewTableCell(tview.Escape(val))
			if c == view.StatusCol {
				cell.SetTextColor(statusColor(val))
			}
			if marked {
				cell.SetBackgroundColor(markColor) // multi-select highlight
			}
			a.table.SetCell(r+1, c, cell)
		}
	}

	// Keep the cursor in range after the row count changes.
	if max := len(rows); row > max {
		row = max
	}
	if row < 1 && len(rows) > 0 {
		row = 1
	}
	a.table.Select(row, 0)
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
		if a.view().Local {
			a.stopSelectedForward() // Port-Fwd view: Enter stops the forward
		} else {
			a.showDetail()
		}
		return nil
	case tcell.KeyEscape:
		if a.filter != "" {
			a.clearFilter() // Esc clears an active filter; no-op otherwise
			return nil
		}
	}

	// Number keys 1..N jump straight to a view.
	if r := event.Rune(); r >= '1' && r <= '9' {
		a.switchView(int(r - '1'))
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
		if a.splash != nil && !a.splashing {
			go a.playSplash() // replay the startup splash
		}
		return nil
	}
	return event
}

func statusColor(status string) tcell.Color {
	switch status {
	case "Running", "Succeeded", "Completed", "Bound", "Normal":
		return tcell.ColorGreen
	case "Pending", "ContainerCreating", "Terminating":
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
