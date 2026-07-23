package ui

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/teknik-github/kcli/internal/config"
)

// Workspaces persist the *shape* of a session — which tabs are open, what each
// points at, and how the screen is split — so a monitoring layout survives a
// restart. Rows are never saved; every restored tab reloads from the cluster.
//
// ":ws save [name]" stores one, ":ws load [name]" restores it, and the
// "default" workspace (if any) is restored automatically at startup. Quitting
// overwrites "last", so a layout closed by accident is one ":ws load last" away.

// snapshotWorkspace captures the current layout. Runs on the UI goroutine.
func (a *App) snapshotWorkspace() config.Workspace {
	a.saveTab() // fold the live fields into the active tab first

	ws := config.Workspace{
		ActiveTab: a.activeTab,
		Split:     a.split,
		Tabs:      make([]config.Tab, 0, len(a.tabList)),
	}
	if a.split != splitOff { // pane bookkeeping is meaningless (and stale) unsplit
		ws.ActivePane = a.activePane
		ws.PaneTabs = append([]int(nil), a.paneTabs[:a.paneCount]...)
	}
	for _, t := range a.tabList {
		st := config.Tab{
			Name:      t.name,
			View:      resourceViews[t.viewIdx].ID,
			Namespace: t.namespace,
			Filter:    t.filter,
			SortCol:   t.sortCol,
			SortDesc:  t.sortDesc,
		}
		if t.viewIdx == a.dynIdx && t.dynValid { // a CRD tab: remember the GVR
			st.Group, st.Version, st.Resource = t.dynGVR.Group, t.dynGVR.Version, t.dynGVR.Resource
			st.Kind, st.Namespaced = t.dynSlot.Title, t.dynNamespaced
		}
		ws.Tabs = append(ws.Tabs, st)
	}
	return ws
}

// applyWorkspace replaces the tab list and split layout with a saved one. Tabs
// whose resource no longer exists are dropped; if nothing survives, the current
// session is kept. Runs on the UI goroutine (or before Run, at startup).
func (a *App) applyWorkspace(ws config.Workspace) bool {
	tabs := make([]*tabState, 0, len(ws.Tabs))
	for _, st := range ws.Tabs {
		t := a.tabFromSaved(st)
		if t == nil {
			continue // resource dropped from the registry, or an unusable entry
		}
		tabs = append(tabs, t)
	}
	if len(tabs) == 0 {
		return false
	}

	a.tabList = tabs
	a.activeTab = ws.ActiveTab
	if a.activeTab < 0 || a.activeTab >= len(tabs) {
		a.activeTab = 0
	}

	// Restore the split only if it still describes 2..maxPanes distinct, existing
	// tabs in a known arrangement.
	a.split, a.activePane, a.paneCount, a.paneTabs = splitOff, 0, 1, [maxPanes]int{}
	if ws.Split > splitOff && ws.Split <= splitGrid && len(ws.PaneTabs) >= 2 &&
		len(ws.PaneTabs) <= maxPanes && len(ws.PaneTabs) <= len(tabs) {
		seen := map[int]bool{}
		ok := true
		for _, i := range ws.PaneTabs {
			if i < 0 || i >= len(tabs) || seen[i] {
				ok = false
				break
			}
			seen[i] = true
		}
		if ok {
			a.split = ws.Split
			a.paneCount = len(ws.PaneTabs)
			copy(a.paneTabs[:], ws.PaneTabs)
			a.activePane = ws.ActivePane
			if a.activePane < 0 || a.activePane >= a.paneCount {
				a.activePane = 0
			}
		}
	}

	a.restoreLive()
	a.fixPanes()
	return true
}

// restoreLive copies the active tab's session into the live App fields — the
// same direction loadTab moves state, without the repaint it does.
func (a *App) restoreLive() {
	t := a.tabList[a.activeTab]
	if t.viewIdx == a.dynIdx && t.dynValid {
		*resourceViews[a.dynIdx] = t.dynSlot // the shared slot follows the active tab
	}
	a.viewIdx = t.viewIdx
	a.prevViewIdx = t.prevViewIdx
	a.namespace = t.namespace
	a.filter = t.filter
	a.sortCol = t.sortCol
	a.sortDesc = t.sortDesc
	a.marked = nil
	a.rows = nil
	a.dynGVR = t.dynGVR
	a.dynNamespaced = t.dynNamespaced
}

// tabFromSaved rebuilds one tab, or nil when its resource is gone.
func (a *App) tabFromSaved(st config.Tab) *tabState {
	t := &tabState{
		name:      st.Name,
		namespace: st.Namespace,
		filter:    st.Filter,
		sortCol:   st.SortCol,
		sortDesc:  st.SortDesc,
	}
	if st.Resource != "" { // a CRD tab: rebuild its own Dynamic slot copy
		gvr := schema.GroupVersionResource{Group: st.Group, Version: st.Version, Resource: st.Resource}
		t.viewIdx = a.dynIdx
		t.dynGVR = gvr
		t.dynNamespaced = st.Namespaced
		t.dynSlot = dynamicViewDef(gvr, st.Namespaced, st.Kind)
		t.dynValid = true
		return t
	}
	i := viewIndexByID(st.View)
	if i < 0 {
		return nil
	}
	t.viewIdx = i
	t.prevViewIdx = i
	return t
}

// restoreStartupWorkspace loads the "default" workspace during NewApp, before
// the event loop runs. Saving under that name is how a user opts in to it.
func (a *App) restoreStartupWorkspace() {
	ws, ok := config.LoadWorkspace(config.DefaultWorkspace)
	if !ok {
		return
	}
	if a.applyWorkspace(ws) {
		a.publishCadence()
		a.rebuildBody()
		a.drawTabChrome()
	}
}

// repaintWorkspace redraws everything a restored layout touches and reloads
// both panes. Runs on the UI goroutine.
func (a *App) repaintWorkspace() {
	a.publishCadence()
	a.table.Clear()
	a.rebuildBody()
	a.drawTabChrome()
	a.drawTabs()
	a.drawHeader()
	a.drawTable()
	a.drawSplitPanes()
	go a.refresh()
}

// workspaceCommand handles the ":ws …" sub-commands. Anything unrecognised
// falls back to listing what is saved.
func (a *App) workspaceCommand(args string) {
	verb, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	name := strings.TrimSpace(rest)
	if name == "" {
		name = config.DefaultWorkspace
	}

	switch strings.ToLower(verb) {
	case "save", "s":
		if err := config.SaveWorkspace(name, a.snapshotWorkspace()); err != nil {
			a.showMessage("ws", fmt.Sprintf("could not save workspace %q: %v", name, err))
			return
		}
		a.showMessage("ws", fmt.Sprintf("workspace %q saved (%d tabs)", name, len(a.tabList)))
	case "load", "l", "open":
		ws, ok := config.LoadWorkspace(name)
		if !ok {
			a.showMessage("ws", fmt.Sprintf("no workspace %q — saved: %s", name, a.workspaceList()))
			return
		}
		if !a.applyWorkspace(ws) {
			a.showMessage("ws", fmt.Sprintf("workspace %q has no usable tabs", name))
			return
		}
		a.repaintWorkspace()
	case "rm", "delete", "del":
		if err := config.DeleteWorkspace(name); err != nil {
			a.showMessage("ws", fmt.Sprintf("could not delete workspace %q: %v", name, err))
			return
		}
		a.showMessage("ws", fmt.Sprintf("workspace %q deleted", name))
	default: // "list", "ls", or a typo
		a.showMessage("ws", fmt.Sprintf("saved workspaces: %s\n\n:ws save|load|rm <name>  (default: %q, restored at startup)",
			a.workspaceList(), config.DefaultWorkspace))
	}
}

// workspaceList renders the saved names for a message.
func (a *App) workspaceList() string {
	names := config.WorkspaceNames()
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// quit saves the layout to the "last" slot, then stops the app. The save is
// best-effort: failing to write a convenience snapshot must not block quitting.
func (a *App) quit() {
	_ = config.SaveWorkspace(config.LastWorkspace, a.snapshotWorkspace())
	a.tv.Stop()
}
