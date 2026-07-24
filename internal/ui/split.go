package ui

import (
	"context"
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// grayPane borders (and labels) the panes that are not focused.
var grayPane = tcell.ColorGray

// Split modes: several tabs on screen at once, as columns, stacked rows, or a
// grid filled two per row (2x2 at four panes).
const (
	splitOff = iota
	splitVert
	splitHoriz
	splitGrid
)

// maxPanes is the ceiling on panes shown at once. Four is where a table still
// has enough width/height to be readable on a normal terminal.
const maxPanes = 4

// Split is a layout over the tab list, not a second session model: each pane
// shows one tab, and the *live* session (the App's mutable fields) is always the
// one in the focused pane.
//
// The trick that keeps this cheap: a.table always renders the live tab, and the
// a.parked tables render the others. Focusing another pane does not move state
// between widgets — it swaps their positions inside the body flex, so the tab
// under the cursor stays put on screen while a.table remains "the live table"
// for every existing read (selectedRow, drawTable, modals refocusing it).
//
// paneTabs[p] is the tab shown at position p (0 = leftmost/topmost, filling in
// reading order); paneCount is how many positions exist; activePane is the
// position holding the live tab, i.e. where a.table is placed.

// assignPaneTables maps each on-screen position to its widget: the live table at
// activePane, the parked tables in order everywhere else. Positions own widgets,
// tabs do not — that is what lets the focus move without any state moving.
func (a *App) assignPaneTables() {
	k := 0
	for p := 0; p < a.paneCount && p < maxPanes; p++ {
		if p == a.activePane {
			a.paneTable[p] = a.table
			continue
		}
		a.paneTable[p] = a.parked[k]
		k++
	}
}

// openPaneOverlay shows p in the focused pane (the whole table area when
// unsplit), leaving the header, tab bars, footer and any other panes on screen.
// The overlay is pinned to that pane position, so `\` can later move the focus
// away without dragging the overlay along.
func (a *App) openPaneOverlay(p tview.Primitive) {
	a.closePaneOverlay() // never stack two; the graph is the only user today
	a.paneOverlay = p
	a.overlayPane = a.activePane
	a.rebuildBody()
}

// closePaneOverlay puts the pane's table back. Safe to call when nothing is
// open. It also stops the graph sampler, since the graph is the only overlay —
// so any code path that dismisses the overlay (a structural key, a view switch)
// tears the sampler down too, without having to know a graph was showing.
func (a *App) closePaneOverlay() {
	if a.paneOverlay == nil {
		return
	}
	a.stopGraph()
	a.paneOverlay = nil
	a.rebuildBody()
	a.drawTable()
}

// overlayVisibleAt reports whether the overlay occupies pane position p.
func (a *App) overlayVisibleAt(p int) bool {
	return a.paneOverlay != nil && p == a.overlayPane
}

// focusTarget is the widget keys should be aimed at: the overlay when the focus
// is sitting on the pane holding it, otherwise the live table. Moving the focus
// to another pane while the graph stays open lands here on the table, so all the
// normal table keys work again.
func (a *App) focusTarget() tview.Primitive {
	if a.paneOverlay != nil && (a.split == splitOff || a.activePane == a.overlayPane) {
		return a.paneOverlay
	}
	return a.table
}

// rebuildBody lays the panes out for the current mode and pane count.
func (a *App) rebuildBody() {
	a.body.Clear()
	if a.split == splitOff {
		a.table.SetBorder(false)
		item := tview.Primitive(a.table)
		if a.paneOverlay != nil {
			item = a.paneOverlay
		}
		a.body.SetDirection(tview.FlexRow).AddItem(item, 0, 1, true)
		a.tv.SetFocus(a.focusTarget())
		return
	}
	a.assignPaneTables()

	if a.split == splitGrid {
		// Two per row, so four panes read as a quadrant and three leave the last
		// one full width.
		a.body.SetDirection(tview.FlexRow)
		for p := 0; p < a.paneCount; p += 2 {
			row := tview.NewFlex().SetDirection(tview.FlexColumn)
			row.AddItem(a.paneWidget(p), 0, 1, false)
			if p+1 < a.paneCount {
				row.AddItem(a.paneWidget(p+1), 0, 1, false)
			}
			a.body.AddItem(row, 0, 1, false)
		}
	} else {
		dir := tview.FlexColumn
		if a.split == splitHoriz {
			dir = tview.FlexRow
		}
		a.body.SetDirection(dir)
		for p := 0; p < a.paneCount; p++ {
			a.body.AddItem(a.paneWidget(p), 0, 1, p == a.activePane)
		}
	}
	a.drawPaneTitles()
	a.tv.SetFocus(a.focusTarget())
}

// paneWidget is the primitive to lay out at position p: the overlay when it is
// pinned there, the live table at the focused pane, otherwise a parked table.
func (a *App) paneWidget(p int) tview.Primitive {
	if a.overlayVisibleAt(p) {
		return a.paneOverlay
	}
	if p == a.activePane {
		return a.table
	}
	return a.paneTable[p]
}

// toggleSplit applies a split arrangement. From unsplit, `|` and `-` open two
// panes and the grid key opens a full quad; pressing the same key again grows
// the split by one pane and folds it away once it is full. Splitting with fewer
// tabs than panes clones the current session to fill them.
func (a *App) toggleSplit(mode int) {
	a.closePaneOverlay() // a layout change invalidates the overlay's pinned pane
	switch {
	case a.split == mode: // same arrangement again: grow it, or fold it away
		if mode == splitGrid || !a.addPane() {
			a.unsplit()
			return
		}
	case a.split == splitOff: // open it
		want := 2
		if mode == splitGrid {
			want = maxPanes
		}
		a.activePane = 0
		a.paneCount = 1
		a.paneTabs[0] = a.activeTab
		a.split = mode // addPane reads the live split when picking tabs
		for a.paneCount < want && a.addPane() {
		}
	}
	a.split = mode
	a.fixPanes()
	a.rebuildBody()
	a.drawTable()
	a.drawTabbar()
	a.drawSplitPanes()
	go a.refresh() // pull fresh rows for the panes that just appeared
}

// addPane puts one more tab on screen, cloning the current session when every
// open tab is already visible. Reports whether a pane was added.
func (a *App) addPane() bool {
	if a.paneCount >= maxPanes {
		return false
	}
	i := a.freeTab()
	if i < 0 {
		i = a.cloneTab()
	}
	a.paneTabs[a.paneCount] = i
	a.paneCount++
	return true
}

// dropPane removes the focused pane, unsplitting at the last two. The tab it
// showed stays open — this closes the pane, not the session (that is `w`).
func (a *App) dropPane() {
	if a.split == splitOff {
		return
	}
	a.closePaneOverlay() // the pane it is pinned to may be the one going away
	if a.paneCount <= 2 {
		a.unsplit()
		return
	}
	p := a.activePane
	copy(a.paneTabs[p:], a.paneTabs[p+1:a.paneCount])
	a.paneCount--
	if p >= a.paneCount {
		p = a.paneCount - 1
	}
	a.saveTab()
	a.activePane = p
	a.loadTab(a.paneTabs[p]) // the pane that took its place owns the focus now
	a.rebuildBody()          // the layout lost a slot, so it always needs rebuilding
	a.drawPaneTitles()
}

// unsplit collapses back to the single full-screen table.
func (a *App) unsplit() {
	a.split = splitOff
	a.paneCount = 1
	a.activePane = 0
	a.rebuildBody()
	a.drawTable()
	a.drawTabbar() // the on-screen marks in the workspace strip are gone now
}

// focusNextPane moves the focus — and with it the live session — to the next
// pane, wrapping around.
func (a *App) focusNextPane() {
	if a.split == splitOff {
		return
	}
	for k := 1; k < a.paneCount; k++ {
		p := (a.activePane + k) % a.paneCount
		t := a.paneTabs[p]
		if t < 0 || t >= len(a.tabList) || t == a.activeTab {
			continue
		}
		a.saveTab()
		a.activePane = p
		a.loadTab(t)
		// The live table widget has to move to the pane that now owns the focus,
		// otherwise the tabs visibly trade places instead of the cursor moving.
		a.rebuildBody()
		a.drawPaneTitles()
		return
	}
}

// fixPanes reconciles the pane assignment after the tab list or the active tab
// changed: every position ends up on a distinct, existing tab, with the live one
// at activePane. It reports whether the pane *order* moved, which is the only
// case that needs the body flex rebuilt.
func (a *App) fixPanes() bool {
	if a.split == splitOff {
		return false
	}
	if len(a.tabList) < 2 { // nothing left to show beside it
		a.split = splitOff
		a.paneCount = 1
		a.activePane = 0
		return true
	}
	moved := false
	if a.paneCount > len(a.tabList) { // fewer sessions than panes: shed the extras
		a.paneCount = len(a.tabList)
		moved = true
	}
	if a.paneCount < 2 {
		a.paneCount = 2
		moved = true
	}
	if a.activePane < 0 || a.activePane >= a.paneCount {
		a.activePane = 0
		moved = true
	}
	// Activating a tab that is already on screen moves the focus to its pane
	// instead of dragging its content across.
	if a.paneTabs[a.activePane] != a.activeTab {
		if p := a.paneOf(a.activeTab); p >= 0 {
			a.activePane = p
			moved = true
		}
	}
	a.paneTabs[a.activePane] = a.activeTab

	used := map[int]bool{a.activeTab: true}
	for p := 0; p < a.paneCount; p++ {
		if p == a.activePane {
			continue
		}
		i := a.paneTabs[p]
		if i < 0 || i >= len(a.tabList) || used[i] {
			i = a.freeTabExcept(used)
		}
		if i < 0 { // no tab left to fill this position
			a.paneCount = p
			moved = true
			break
		}
		a.paneTabs[p] = i
		used[i] = true
	}
	if a.activePane >= a.paneCount { // the shed positions took the live one with them
		a.activePane = 0
		a.paneTabs[0] = a.activeTab
		moved = true
	}
	return moved
}

// paneOf returns the position showing tab i, or -1 when it is off screen.
func (a *App) paneOf(i int) int {
	for p := 0; p < a.paneCount; p++ {
		if a.paneTabs[p] == i {
			return p
		}
	}
	return -1
}

// freeTab returns a tab that is not on screen, or -1 when all of them are.
func (a *App) freeTab() int {
	used := map[int]bool{}
	for p := 0; p < a.paneCount; p++ {
		used[a.paneTabs[p]] = true
	}
	return a.freeTabExcept(used)
}

// freeTabExcept returns the first tab not in used, or -1.
func (a *App) freeTabExcept(used map[int]bool) int {
	for i := range a.tabList {
		if !used[i] {
			return i
		}
	}
	return -1
}

// paneTabSet reports which tabs are on screen in a parked pane.
func (a *App) paneTabSet() map[int]bool {
	on := map[int]bool{}
	if a.split == splitOff {
		return on
	}
	for p := 0; p < a.paneCount; p++ {
		if p != a.activePane {
			on[a.paneTabs[p]] = true
		}
	}
	return on
}

// paneTab returns the parked tab shown at position p.
func (a *App) paneTab(p int) (*tabState, bool) {
	if a.split == splitOff || p < 0 || p >= a.paneCount || p == a.activePane {
		return nil, false
	}
	i := a.paneTabs[p]
	if i < 0 || i >= len(a.tabList) || i == a.activeTab {
		return nil, false
	}
	return a.tabList[i], true
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

// drawSplitPanes repaints every parked pane from its tabState — same filter,
// sort and marks it would have if it were live.
func (a *App) drawSplitPanes() {
	if a.split == splitOff {
		return
	}
	for p := 0; p < a.paneCount; p++ {
		a.drawPane(p)
	}
}

// drawPane repaints one parked pane.
func (a *App) drawPane(p int) {
	t, ok := a.paneTab(p)
	if !ok {
		return
	}
	view, ok := a.tabView(t)
	if !ok {
		return
	}
	tbl := a.paneTable[p]
	if tbl == nil {
		return
	}
	rows := filterSortRows(t.rows, t.filter, t.sortCol, t.sortDesc)
	drawRows(tbl, view, rows, t.marked)
	if t.selRow > 0 && t.selRow <= len(rows) {
		tbl.Select(t.selRow, 0)
	}
	a.paneTitle(tbl, a.paneTabs[p], false)
}

// loadSplitPanes fetches every parked pane's rows on the same cadence as the
// live view.
func (a *App) loadSplitPanes() {
	if a.split == splitOff {
		return
	}
	for p := 0; p < a.paneCount; p++ {
		a.loadPane(p)
	}
}

// loadPane fetches one parked pane. It follows the usual rules: view/namespace/
// client captured here on the UI goroutine, the fetch on a background one, the
// store back inside QueueUpdateDraw and dropped if the pane moved meanwhile.
func (a *App) loadPane(p int) {
	t, ok := a.paneTab(p)
	if !ok {
		return
	}
	view, ok := a.tabView(t)
	if !ok {
		return
	}
	if view.Local { // Port-Fwd / Bench: rows come from App state, never the cluster
		t.rows = localRows(view, a)
		a.drawPane(p)
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
			if cur, ok := a.paneTab(p); !ok || cur != t {
				return // the pane now shows something else
			}
			t.rows = rows
			a.drawPane(p)
		})
	}()
}

// drawPaneTitles borders every pane with its tab label; the live pane is
// accented, the parked ones grey, so it is obvious which one the keys drive.
func (a *App) drawPaneTitles() {
	if a.split == splitOff {
		a.table.SetBorder(false)
		return
	}
	a.assignPaneTables()
	for p := 0; p < a.paneCount; p++ {
		if a.overlayVisibleAt(p) {
			// The overlay draws its own border; just tint it to match whether it is
			// the focused pane, so `\` reads the same as it does on the tables.
			a.tintOverlayBorder(p == a.activePane)
			continue
		}
		if tbl := a.paneTable[p]; tbl != nil {
			a.paneTitle(tbl, a.paneTabs[p], p == a.activePane)
		}
	}
}

// tintOverlayBorder colours the overlay's border accent when it is the focused
// pane, grey otherwise — matching the tables. It is best-effort: only a Box-
// backed primitive (the graph's TextView is one) can be tinted.
func (a *App) tintOverlayBorder(focused bool) {
	type bordered interface {
		SetBorderColor(tcell.Color) *tview.Box
		SetTitleColor(tcell.Color) *tview.Box
	}
	b, ok := a.paneOverlay.(bordered)
	if !ok {
		return
	}
	color := accentColor(a.accent)
	if !focused {
		color = grayPane
	}
	b.SetBorderColor(color).SetTitleColor(color)
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
