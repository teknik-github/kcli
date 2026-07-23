package ui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// tabState is one browser-style tab: a self-contained view session. The active
// tab's session lives in the App's mutable fields (viewIdx, namespace, filter,
// …); the rest sit parked here until activated. This keeps every existing
// read/write of a.viewIdx/a.namespace/… untouched — only tab switches move state
// between the live fields and a tabState.
type tabState struct {
	viewIdx     int
	prevViewIdx int
	namespace   string
	filter      string
	sortCol     int
	sortDesc    bool
	marked      map[string]bool
	rows        []Row
	selRow      int    // remembered cursor row, restored on activate
	name        string // user-set label; overrides the auto resource/ns title when non-empty

	// Dynamic (CRD) view snapshot. The Dynamic slot in resourceViews is a single
	// shared struct that :jump rewrites, so a tab parked on a CRD must carry its
	// own copy and write it back when reactivated.
	dynGVR        schema.GroupVersionResource
	dynNamespaced bool
	dynSlot       viewDef // copy of resourceViews[dynIdx] for this tab
	dynValid      bool    // dynSlot has been populated
}

// saveTab snapshots the live session into the active tab. Runs on the UI
// goroutine (like every tab op).
func (a *App) saveTab() {
	t := a.tabList[a.activeTab]
	t.viewIdx = a.viewIdx
	t.prevViewIdx = a.prevViewIdx
	t.namespace = a.namespace
	t.filter = a.filter
	t.sortCol = a.sortCol
	t.sortDesc = a.sortDesc
	t.marked = a.marked
	t.rows = a.rows
	t.dynGVR = a.dynGVR
	t.dynNamespaced = a.dynNamespaced
	if r, _ := a.table.GetSelection(); r > 0 {
		t.selRow = r
	}
	if a.viewIdx == a.dynIdx {
		t.dynSlot = *resourceViews[a.dynIdx]
		t.dynValid = true
	}
}

// loadTab restores tab i into the live session and repaints. The cluster fetch
// runs afterwards so any staleness (e.g. after a context switch on another tab)
// is reconciled.
func (a *App) loadTab(i int) {
	if i < 0 || i >= len(a.tabList) {
		return
	}
	a.activeTab = i
	t := a.tabList[i]

	// Restore this tab's CRD target into the shared Dynamic slot before anything
	// reads resourceViews[dynIdx].
	if t.viewIdx == a.dynIdx && t.dynValid {
		*resourceViews[a.dynIdx] = t.dynSlot
	}

	a.viewIdx = t.viewIdx
	a.prevViewIdx = t.prevViewIdx
	a.namespace = t.namespace
	a.filter = t.filter
	a.sortCol = t.sortCol
	a.sortDesc = t.sortDesc
	a.marked = t.marked
	a.rows = t.rows
	a.dynGVR = t.dynGVR
	a.dynNamespaced = t.dynNamespaced

	a.publishCadence()
	a.table.Clear()
	// Reassign the panes before painting: the incoming tab may already be on
	// screen in the other pane, in which case the panes swap position.
	if a.fixPanes() {
		a.rebuildBody()
	}
	a.drawPaneTitles()
	a.drawTabbar()
	a.drawTabs()
	a.drawHeader()
	a.drawTable()
	a.drawSplitPanes()
	if t.selRow > 0 {
		a.table.Select(t.selRow, 0)
	}
	go a.refresh()
}

// newTab opens a fresh tab cloning the current view and namespace (so it starts
// on something useful) but with its own filter/sort/selection/marks.
func (a *App) newTab() {
	a.loadTab(a.cloneTab())
}

// cloneTab appends a tab cloning the current session and returns its index,
// without activating it — that is what lets a new split pane be filled without
// stealing the focus mid-layout.
func (a *App) cloneTab() int {
	a.saveTab()
	nt := &tabState{
		viewIdx:       a.viewIdx,
		prevViewIdx:   a.prevViewIdx,
		namespace:     a.namespace,
		sortCol:       -1,
		dynGVR:        a.dynGVR,
		dynNamespaced: a.dynNamespaced,
	}
	if a.viewIdx == a.dynIdx {
		nt.dynSlot = *resourceViews[a.dynIdx]
		nt.dynValid = true
	}
	a.tabList = append(a.tabList, nt)
	return len(a.tabList) - 1
}

// closeTab drops the active tab, keeping at least one open.
func (a *App) closeTab() {
	if len(a.tabList) <= 1 {
		return
	}
	i := a.activeTab
	a.tabList = append(a.tabList[:i], a.tabList[i+1:]...)
	// Removing a tab shifts every later index: fix up what the panes point at
	// (-1 means "gone", which fixPanes re-picks) before loading.
	for p := range a.paneTabs {
		switch {
		case a.paneTabs[p] == i:
			a.paneTabs[p] = -1
		case a.paneTabs[p] > i:
			a.paneTabs[p]--
		}
	}
	if i >= len(a.tabList) {
		i = len(a.tabList) - 1
	}
	a.loadTab(i)
}

// nextTab / prevTab cycle with wraparound; gotoTab jumps to tab n (0-based).
func (a *App) nextTab() {
	if len(a.tabList) < 2 {
		return
	}
	a.saveTab()
	a.loadTab((a.activeTab + 1) % len(a.tabList))
}

func (a *App) prevTab() {
	if len(a.tabList) < 2 {
		return
	}
	a.saveTab()
	a.loadTab((a.activeTab - 1 + len(a.tabList)) % len(a.tabList))
}

func (a *App) gotoTab(n int) {
	if n < 0 || n >= len(a.tabList) || n == a.activeTab {
		return
	}
	a.saveTab()
	a.loadTab(n)
}

// drawTabbar renders the workspace strip and sizes its row: hidden (height 0)
// with a single, unnamed tab; one line once there are multiple tabs or the sole
// tab has been given a custom name.
func (a *App) drawTabbar() {
	if !a.tabbarVisible() {
		a.tabbar.SetText("")
		a.flex.ResizeItem(a.tabbar, 0, 0)
		return
	}
	a.flex.ResizeItem(a.tabbar, 1, 0)

	onScreen := a.paneTabSet() // the tabs sharing the screen, if any

	var b strings.Builder
	for i := range a.tabList {
		label := fmt.Sprintf(" %d:%s ", i+1, a.tabTitle(i))
		switch {
		case i == a.activeTab:
			fmt.Fprintf(&b, "[%s:%s:b]%s[-:-:-] ", a.accentTextTag(), a.accent, label)
		case onScreen[i]: // also on screen, just not focused
			fmt.Fprintf(&b, "[%s::u]%s[-:-:-] ", a.accent, label)
		default:
			fmt.Fprintf(&b, "[%s]%s[-] ", a.accent, label)
		}
	}
	a.tabbar.SetText(b.String())
}

// drawTabChrome repaints everything that carries a tab's label: the workspace
// strip and, while split, the pane borders. Call it after anything that changes
// what a tab is showing — a view switch, a namespace change, a rename — since
// those all move the auto title.
func (a *App) drawTabChrome() {
	a.drawTabbar()
	a.drawPaneTitles()
}

// tabbarVisible reports whether the workspace strip should show: with more than
// one tab, or when the single tab carries a custom name worth surfacing.
func (a *App) tabbarVisible() bool {
	return len(a.tabList) > 1 || a.tabList[0].name != ""
}

// tabTitle labels a tab. A user-set name wins; otherwise it is the resource and
// namespace. The active tab reads live state; parked tabs read their snapshot
// (and a parked CRD tab its dynSlot, since the shared Dynamic slot currently
// holds some other tab's target).
func (a *App) tabTitle(i int) string {
	if n := a.tabList[i].name; n != "" {
		return n
	}
	if i == a.activeTab {
		return withNamespace(a.view().Title, a.namespace, a.view().ClusterScoped)
	}
	t := a.tabList[i]
	title := resourceViews[t.viewIdx].Title
	cluster := resourceViews[t.viewIdx].ClusterScoped
	if t.viewIdx == a.dynIdx && t.dynValid {
		title, cluster = t.dynSlot.Title, t.dynSlot.ClusterScoped
	}
	return withNamespace(title, t.namespace, cluster)
}

// renameTab prompts for a custom label for the active tab. An empty name clears
// the override, reverting to the auto resource/namespace title.
func (a *App) renameTab() {
	input := tview.NewInputField().
		SetLabel(" name: ").
		SetText(a.tabList[a.activeTab].name).
		SetFieldWidth(24)
	// Use the InputField directly (not wrapped in a Form): a Form intercepts Enter
	// for field navigation, which would keep the modal open. SetDoneFunc on the
	// bare field gives a clean Enter=apply / Esc=cancel.
	input.SetBorder(true).SetTitle(" rename tab  (empty = auto) ")
	input.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			a.tabList[a.activeTab].name = strings.TrimSpace(input.GetText())
			a.drawTabChrome()
		}
		a.closeModal("rename")
	})
	a.openModal("rename", input, 40, 3)
}

// withNamespace appends the namespace to a tab label unless the resource is
// cluster-scoped or the tab spans all namespaces.
func withNamespace(title, ns string, clusterScoped bool) string {
	if clusterScoped || ns == "" {
		return title
	}
	return title + "/" + ns
}
