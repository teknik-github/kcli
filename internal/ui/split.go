package ui

import (
	"context"
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// grayPane borders (and labels) the pane that is not focused.
var grayPane = tcell.ColorGray

// Split modes: two tabs on screen at once, side by side or stacked.
const (
	splitOff = iota
	splitVert
	splitHoriz
)

// Split is a layout over the tab list, not a second session model: each pane
// shows one tab, and the *live* session (the App's mutable fields) is always the
// one in the focused pane.
//
// The trick that keeps this cheap: a.table always renders the live tab, and
// a.table2 always renders the parked one. Focusing the other pane does not move
// state between widgets — it swaps their positions inside the body flex, so the
// tab under the cursor stays put on screen while a.table remains "the live
// table" for every existing read (selectedRow, drawTable, modals refocusing it).
//
// rebuildBody lays the panes out. paneTabs[p] is the tab shown at position p
// (0 = left/top, 1 = right/bottom); activePane is the position holding the live
// tab, i.e. where a.table is placed.
func (a *App) rebuildBody() {
	a.body.Clear()
	if a.split == splitOff {
		a.table.SetBorder(false)
		a.body.SetDirection(tview.FlexRow).AddItem(a.table, 0, 1, true)
		a.tv.SetFocus(a.table)
		return
	}
	dir := tview.FlexColumn
	if a.split == splitHoriz {
		dir = tview.FlexRow
	}
	a.body.SetDirection(dir)
	for p := 0; p < 2; p++ {
		tbl := a.table2
		if p == a.activePane {
			tbl = a.table
		}
		a.body.AddItem(tbl, 0, 1, p == a.activePane)
	}
	a.drawPaneTitles()
	a.tv.SetFocus(a.table) // keys always drive the live pane
}

// toggleSplit turns the split on in the given mode, switches orientation, or
// turns it off when the same mode is pressed again. Splitting with a single tab
// open clones it, so `|` alone is enough to get two panes.
func (a *App) toggleSplit(mode int) {
	if a.split == mode {
		a.split = splitOff
		a.rebuildBody()
		a.drawTable()
		return
	}
	if a.split == splitOff {
		if len(a.tabList) < 2 {
			a.newTab() // clone the current session into a second tab to fill pane 2
		}
		a.activePane = 0
		a.paneTabs[0] = a.activeTab
		a.paneTabs[1] = a.otherTab()
	}
	a.split = mode
	a.fixPanes()
	a.rebuildBody()
	a.drawTable()
	a.drawTabbar()
	a.drawSplitPane()
	go a.refresh() // pull fresh rows for the pane that just appeared
}

// otherTab picks a tab to put in the second pane: the one after the active tab,
// wrapping around.
func (a *App) otherTab() int {
	if len(a.tabList) < 2 {
		return a.activeTab
	}
	return (a.activeTab + 1) % len(a.tabList)
}

// swapPane moves the focus (and with it the live session) to the other pane.
func (a *App) swapPane() {
	if a.split == splitOff {
		return
	}
	target := a.paneTabs[1-a.activePane]
	if target < 0 || target >= len(a.tabList) || target == a.activeTab {
		return
	}
	a.saveTab()
	a.activePane = 1 - a.activePane
	a.loadTab(target)
	// The live table widget has to move to the pane that now owns the focus,
	// otherwise the two tabs visibly trade places instead of the cursor moving.
	a.rebuildBody()
	a.drawPaneTitles()
}

// fixPanes reconciles the pane assignment after the tab list or the active tab
// changed. It reports whether the pane *order* moved, which is the only case
// that needs the body flex rebuilt.
func (a *App) fixPanes() bool {
	if a.split == splitOff {
		return false
	}
	if len(a.tabList) < 2 { // nothing left to show beside it
		a.split = splitOff
		return true
	}
	moved := false
	// Activating the tab that already sits in the other pane moves the focus
	// there instead of dragging its content across.
	if a.activeTab == a.paneTabs[1-a.activePane] {
		a.activePane = 1 - a.activePane
		moved = true
	}
	a.paneTabs[a.activePane] = a.activeTab

	other := a.paneTabs[1-a.activePane]
	if other < 0 || other >= len(a.tabList) || other == a.activeTab {
		for i := range a.tabList { // fall back to any other tab
			if i != a.activeTab {
				a.paneTabs[1-a.activePane] = i
				break
			}
		}
	}
	return moved
}

// splitPaneTab returns the parked tab currently shown in the other pane.
func (a *App) splitPaneTab() (*tabState, int, bool) {
	if a.split == splitOff {
		return nil, -1, false
	}
	i := a.paneTabs[1-a.activePane]
	if i < 0 || i >= len(a.tabList) || i == a.activeTab {
		return nil, -1, false
	}
	return a.tabList[i], i, true
}

// tabView resolves the viewDef a parked tab points at. A tab parked on a CRD
// must use its own dynSlot copy: the shared Dynamic slot currently describes
// whichever tab last jumped there.
func (a *App) tabView(t *tabState) (*viewDef, bool) {
	if t.viewIdx == a.dynIdx {
		if !t.dynValid {
			return nil, false
		}
		return &t.dynSlot, true
	}
	if t.viewIdx < 0 || t.viewIdx >= len(resourceViews) {
		return nil, false
	}
	return resourceViews[t.viewIdx], true
}

// drawSplitPane repaints the parked pane from its tabState — same filter, sort
// and marks it would have if it were live.
func (a *App) drawSplitPane() {
	t, i, ok := a.splitPaneTab()
	if !ok {
		return
	}
	view, ok := a.tabView(t)
	if !ok {
		return
	}
	rows := filterSortRows(t.rows, t.filter, t.sortCol, t.sortDesc)
	drawRows(a.table2, view, rows, t.marked)
	if t.selRow > 0 && t.selRow <= len(rows) {
		a.table2.Select(t.selRow, 0)
	}
	a.paneTitle(a.table2, i, false)
}

// loadSplitPane fetches the parked pane's rows on the same cadence as the live
// view. It follows the usual rules: view/namespace/client captured here on the
// UI goroutine, the fetch on a background one, the store back inside
// QueueUpdateDraw and dropped if the pane moved meanwhile.
func (a *App) loadSplitPane() {
	t, i, ok := a.splitPaneTab()
	if !ok {
		return
	}
	view, ok := a.tabView(t)
	if !ok {
		return
	}
	if view.Local { // Port-Fwd: rows come from App state, never the cluster
		t.rows = a.forwardRows()
		a.drawSplitPane()
		return
	}
	ns := t.namespace
	if view.ClusterScoped {
		ns = ""
	}
	fetch := view.Fetch
	gen := a.clientGen
	cl := a.client

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := fetch(ctx, cl, ns)
		a.tv.QueueUpdateDraw(func() {
			if err != nil || a.clientGen != gen {
				return // stale client, or a failure the live pane will surface too
			}
			if _, cur, ok := a.splitPaneTab(); !ok || cur != i {
				return // the pane now shows something else
			}
			t.rows = rows
			a.drawSplitPane()
		})
	}()
}

// drawPaneTitles borders both panes with their tab labels; the live pane is
// accented, the parked one grey, so it is obvious which one the keys drive.
func (a *App) drawPaneTitles() {
	if a.split == splitOff {
		a.table.SetBorder(false)
		return
	}
	a.paneTitle(a.table, a.activeTab, true)
	if _, i, ok := a.splitPaneTab(); ok {
		a.paneTitle(a.table2, i, false)
	}
}

// paneTitle borders one pane and labels it with its tab number and title.
func (a *App) paneTitle(tbl *tview.Table, tabIdx int, live bool) {
	if tabIdx < 0 || tabIdx >= len(a.tabList) {
		return
	}
	title := fmt.Sprintf(" %d:%s ", tabIdx+1, a.tabTitle(tabIdx))
	color := accentColor(a.accent)
	if !live {
		color = grayPane
		title += `(\ to focus) ` // parens, not brackets: titles parse colour tags
	}
	tbl.SetBorder(true).SetTitle(title).SetTitleColor(color).SetBorderColor(color)
}
