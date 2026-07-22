// Package ui builds the interactive terminal interface with tview/tcell.
package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/teknik-github/kcli/internal/config"
	"github.com/teknik-github/kcli/internal/k8s"
)

// App holds the tview application and the mutable UI state.
type App struct {
	tv     *tview.Application
	client *k8s.Client

	pages  *tview.Pages
	flex   *tview.Flex // root row layout; its tabbar item is resized to 0 with one tab
	logo   *tview.TextView
	header *tview.TextView
	tabbar *tview.TextView // workspace (multi-tab) strip; hidden when only one tab
	tabs   *tview.TextView
	table  *tview.Table
	footer *tview.TextView

	// Browser-style tabs: each holds an independent view session (resource,
	// namespace, filter, sort, marks, rows, cursor). The active tab's session is
	// the live App fields below; switching tabs snapshots the live fields into the
	// outgoing tab and restores the incoming one (see tabs.go).
	tabList   []*tabState
	activeTab int

	viewIdx     int    // index into resourceViews
	prevViewIdx int    // view to return to when leaving a hidden (Local) view
	namespace   string // "" means all namespaces
	filter      string // case-insensitive substring on name/namespace; "" = all
	sortCol     int    // column index to sort by; -1 = fetch order
	sortDesc    bool   // descending when true
	clientGen   int    // bumped on context switch; drops stale in-flight refreshes

	// refreshEvery is the active view's auto-refresh cadence in nanoseconds. It
	// is the one field the autoRefresh ticker goroutine reads, so it is atomic;
	// switchView (UI goroutine) publishes it. Everything else the loop needs is
	// read on the UI goroutine inside loadCurrentView.
	refreshEvery atomic.Int64

	graphStop  chan struct{}      // stops the live graph sampler when non-nil
	logsCancel context.CancelFunc // cancels the active log stream when non-nil
	pflogStop  chan struct{}      // stops the port-forward log view refresh when non-nil

	splash           *splashAnim   // decoded GIF, nil unless KCLI_SPLASH is set
	splashView       *splashView   // active corner animation primitive while showing
	splashing        bool          // true while the corner animation is on screen
	splashStop       chan struct{} // closed to stop the corner animation loop
	splashW, splashH int           // corner box size in cells (KCLI_SPLASH_SIZE)
	splashMode       int           // glyph mode (KCLI_SPLASH_MODE): quadrant/sextant
	sixelEnabled     bool          // KCLI_SPLASH_SIXEL: enable full-screen Sixel playback

	rows      []Row          // current view's data, in fetch order
	forwards  []*portForward // active background port-forwards
	nextFwdID int

	marked map[string]bool // rowKey set for multi-select bulk actions; per-view

	// Dynamic (generic/CRD) view state. dynIdx is the reserved Hidden slot in
	// resourceViews whose fields get rewritten on each :jump to an unregistered
	// resource; the rest describe what that slot currently points at.
	dynIdx        int
	dynGVR        schema.GroupVersionResource
	dynNamespaced bool

	// From the user config file (all with sane defaults when unset).
	baseRefresh time.Duration     // auto-refresh cadence floor
	accent      string            // tview colour name for tabs/header highlights
	userAliases map[string]string // custom :jump aliases -> resource name

	// Live updates: informers call onChange (from their own goroutine) on any
	// watched-resource change; onChange nudges watchTrigger, and watchLoop
	// debounces those nudges into a refresh. See internal/k8s/informer.go.
	onChange     func()
	watchTrigger chan struct{}
}

// NewApp wires up the widget tree and key bindings. cfg may be nil; it carries
// the optional user config (default namespace, refresh cadence, accent, aliases).
func NewApp(client *k8s.Client, cfg *config.Config) *App {
	if cfg == nil {
		cfg = &config.Config{}
	}
	a := &App{
		tv:          tview.NewApplication(),
		client:      client,
		viewIdx:     0,             // start on the first view (Pods)
		namespace:   cfg.Namespace, // configured startup namespace ("" = all)
		sortCol:     -1,            // fetch order until the user sorts
		baseRefresh: cfg.Refresh(), // configured cadence (>= 1s, default 3s)
		accent:      cfg.Accent(),  // configured accent colour name
		userAliases: cfg.NormalizedAliases(),
	}
	a.dynIdx = len(resourceViews) - 1 // the reserved Dynamic slot is appended last
	a.refreshEvery.Store(int64(a.baseRefresh))

	a.logo = tview.NewTextView().SetDynamicColors(true).SetWrap(false).
		SetTextAlign(tview.AlignRight) // pin the banner to the top-right corner, k9s-style
	a.logo.SetText(logoBanner())
	a.header = tview.NewTextView().SetDynamicColors(true)
	a.tabbar = tview.NewTextView().SetDynamicColors(true)
	a.tabs = tview.NewTextView().SetDynamicColors(true)
	a.footer = tview.NewTextView().SetDynamicColors(true)
	a.footer.SetText(footerHelp)

	a.table = tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0)
	a.table.SetSelectedStyle(tcell.StyleDefault.
		Background(accentColor(a.accent)).Foreground(tcell.ColorWhite))

	// Top band, k9s-style: the info block grows to fill the left while the logo
	// keeps its natural width pinned to the right. The band is as tall as the
	// banner so the stacked info lines fill the left side.
	top := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(a.header, 0, 1, false).
		AddItem(a.logo, utf8.RuneCountInString(logoLines[0]), 0, false)

	// The tabbar row starts at height 0 (single tab); drawTabbar resizes it to 1
	// once a second tab exists, so single-tab users see no extra chrome.
	a.flex = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(top, len(logoLines), 0, false).
		AddItem(a.tabbar, 0, 0, false).
		AddItem(a.tabs, 1, 0, false).
		AddItem(a.table, 0, 1, true).
		AddItem(a.footer, 1, 0, false)

	// Start with a single tab holding the initial session.
	a.tabList = []*tabState{{sortCol: -1, namespace: a.namespace}}

	a.pages = tview.NewPages().AddPage("main", a.flex, true, true)

	a.table.SetInputCapture(a.onTableKey)
	a.tv.SetRoot(a.pages, true).SetFocus(a.table)

	// Live updates: a bounded trigger channel coalesces informer callbacks.
	a.watchTrigger = make(chan struct{}, 1)
	a.onChange = func() {
		select {
		case a.watchTrigger <- struct{}{}:
		default: // a refresh is already pending; nothing to add
		}
	}
	client.SetOnChange(a.onChange)

	// Optional startup splash: a GIF path in $KCLI_SPLASH is decoded up front so
	// it can play the moment the event loop starts. A bad/missing file is
	// silently ignored — the splash is cosmetic.
	if p := os.Getenv("KCLI_SPLASH"); p != "" {
		if anim, err := loadGIF(p); err == nil {
			a.splash = anim
			a.splashW, a.splashH = splashSize()
			a.splashMode = splashMode()
			a.sixelEnabled = os.Getenv("KCLI_SPLASH_SIXEL") == "1"
		} else {
			// Surface why the splash won't show instead of failing silently;
			// printed before the TUI starts, so it's visible after quitting.
			fmt.Fprintf(os.Stderr, "kcli: KCLI_SPLASH %q could not be loaded: %v\n", p, err)
		}
	}

	return a
}

// Run starts the event loop and background refresh.
//
// The refresh loop runs in its own goroutine: refresh() calls
// QueueUpdateDraw, which blocks until the tview event loop drains it. That
// loop only runs once tv.Run() is executing, so the first refresh must not be
// called synchronously before tv.Run() or it deadlocks.
func (a *App) Run() error {
	go a.autoRefresh()
	go a.watchLoop()
	if a.splash != nil {
		// Start the corner animation once the event loop is running (QueueUpdateDraw
		// blocks until then, so it can't be called synchronously before tv.Run).
		go func() { a.tv.QueueUpdateDraw(a.startSplash) }()
	}
	return a.tv.Run()
}

// watchLoop turns debounced informer nudges into refreshes. It coalesces a burst
// of change events (e.g. the flood an informer emits on initial sync) into a
// single reload shortly after they settle.
func (a *App) watchLoop() {
	for range a.watchTrigger {
		time.Sleep(400 * time.Millisecond) // let a burst settle
		select {                           // drop any nudge that arrived meanwhile
		case <-a.watchTrigger:
		default:
		}
		a.refresh()
	}
}

// autoRefresh loads the current view once immediately, then polls on the base
// interval, refreshing only once the active view's own cadence has elapsed.
// Views may set RefreshInterval to poll less often (e.g. Events); switching to
// a faster view stays responsive because switchView triggers its own reload.
func (a *App) autoRefresh() {
	a.refresh()
	ticker := time.NewTicker(a.baseRefresh)
	defer ticker.Stop()
	var elapsed time.Duration
	for range ticker.C {
		elapsed += a.baseRefresh
		want := time.Duration(a.refreshEvery.Load())
		if want < a.baseRefresh {
			want = a.baseRefresh
		}
		if elapsed >= want {
			elapsed = 0
			a.refresh()
		}
	}
}

// refresh reloads the current view and redraws. It is safe to call from any
// goroutine EXCEPT the UI goroutine itself (QueueUpdate would deadlock); all
// callers spawn it (go a.refresh()) or run on a background goroutine.
//
// The shared UI state the reload depends on — view index, client, namespace — is
// read inside loadCurrentView on the UI goroutine, not here, so a concurrent
// context switch (which reassigns a.client on the UI goroutine) cannot race with
// it.
func (a *App) refresh() {
	// QueueUpdateDraw (not QueueUpdate) so the redraw happens after loadCurrentView
	// runs: the Local (Port-Fwd) branch updates rows synchronously and would
	// otherwise leave the screen stale until the next input event. Cluster views
	// redraw again from their async fetch — one extra repaint, harmless.
	a.tv.QueueUpdateDraw(a.loadCurrentView)
}

// loadCurrentView captures the active view, client, and namespace on the UI
// goroutine, then fans the cluster fetch out to a background goroutine and
// stores the result back on the UI goroutine. Must run on the UI goroutine (it
// is only ever invoked via QueueUpdate).
func (a *App) loadCurrentView() {
	idx := a.viewIdx
	gen := a.clientGen
	cl := a.client
	view := resourceViews[idx]

	// Local views (port-forwards) are backed by App state, not the cluster.
	if view.Local {
		a.rows = a.forwardRows()
		a.drawHeader()
		a.drawTabs()
		a.drawTable()
		return
	}

	ns := a.namespace
	if view.ClusterScoped {
		ns = ""
	}
	fetch := view.Fetch // capture: the Dynamic slot's Fetch may be reassigned on a later :jump

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := fetch(ctx, cl, ns)
		a.tv.QueueUpdateDraw(func() {
			if a.viewIdx != idx || a.clientGen != gen {
				return // view or context switched while this load was in flight
			}
			if err != nil {
				a.setHeaderError(err)
				return
			}
			a.rows = rows
			a.drawHeader()
			a.drawTabs()
			a.drawTable()
		})
	}()
}

// switchView activates the view at index i and triggers an immediate load.
func (a *App) switchView(i int) {
	if i < 0 || i >= len(resourceViews) || i == a.viewIdx {
		return
	}
	a.viewIdx = i
	a.rows = nil
	a.filter = ""  // filter is per-view; a Services filter must not hide Port-Fwd rows
	a.sortCol = -1 // sort is per-view; reset on switch
	a.sortDesc = false
	a.clearMarks() // marks are per-view identities
	a.publishCadence()
	a.table.Clear()
	a.drawTabs()
	a.drawHeader()
	go a.refresh()
}

// publishCadence stores the active view's auto-refresh interval for the ticker
// goroutine to read. Called on the UI goroutine whenever the view changes.
func (a *App) publishCadence() {
	want := a.baseRefresh
	if iv := resourceViews[a.viewIdx].RefreshInterval; iv > want {
		want = iv
	}
	a.refreshEvery.Store(int64(want))
}

// cycleView moves to the next/previous non-hidden view, wrapping around.
// Hidden (Local) views like Port-Fwd are skipped; reach them via their own key.
func (a *App) cycleView(delta int) {
	n := len(resourceViews)
	i := a.viewIdx
	for k := 0; k < n; k++ {
		i = (i + delta + n) % n
		if !resourceViews[i].Local && !resourceViews[i].Hidden {
			a.switchView(i)
			return
		}
	}
}

// drawHeader renders the top-left info block as stacked "Label: value" lines,
// k9s-style, filling the band beside the logo. Optional lines (filter, sort,
// port-forwards) only appear when active.
func (a *App) drawHeader() {
	ns := a.namespace
	if ns == "" {
		ns = "<all>"
	}
	if a.view().ClusterScoped {
		ns = "-" // namespace is irrelevant for cluster-scoped resources
	}
	lines := []string{
		a.hdrLine("Context", a.client.Context),
		a.hdrLine("Namespace", ns),
		a.hdrLine("Resource", fmt.Sprintf("%s (%d)", a.view().Title, a.rowCount())),
	}
	if a.filter != "" {
		lines = append(lines, a.hdrLine("Filter", a.filter))
	}
	if s := a.sortLabel(); s != "" {
		lines = append(lines, a.hdrLine("Sort", s))
	}
	if n := len(a.marked); n > 0 {
		lines = append(lines, a.hdrLine("Marked", itoa(n)))
	}
	if n := len(a.forwards); n > 0 {
		lines = append(lines, a.hdrLine("Forwards", fmt.Sprintf("⇄ %d", n)))
	}
	a.header.SetText(strings.Join(lines, "\n"))
}

// hdrLine formats one info-block row with the label padded to a fixed width so
// the values line up in a column. The label uses the configured accent colour.
func (a *App) hdrLine(label, value string) string {
	return fmt.Sprintf("[%s::b]%-10s[-::-] [green]%s[-]", a.accent, label+":", value)
}

// accentColor maps a tview colour name to a tcell.Color for widget styling
// (the selected-row background), falling back to dark cyan on an unknown name.
func accentColor(name string) tcell.Color {
	if c := tcell.GetColor(name); c != tcell.ColorDefault {
		return c
	}
	return tcell.ColorDarkCyan
}

// drawTabs renders the resource tab bar with the active view highlighted.
// Hidden (Local) views are omitted; when one is active it shows its own label.
func (a *App) drawTabs() {
	line := ""
	for i, v := range resourceViews {
		if v.Local || v.Hidden {
			continue
		}
		// Only views 0..8 have a working number key (1..9); past that the label
		// drops the number so it does not imply a shortcut that isn't there —
		// reach those via ":" command-jump or Tab.
		label := fmt.Sprintf(" %s ", v.Title)
		if i < 9 {
			label = fmt.Sprintf(" %d:%s ", i+1, v.Title)
		}
		if i == a.viewIdx {
			line += fmt.Sprintf("[black:%s:b]%s[-:-:-]", a.accent, label)
		} else {
			line += fmt.Sprintf("[%s]%s[-]", a.accent, label)
		}
		line += " "
	}
	switch {
	case a.view().Local: // e.g. Port-Fwd: show it separately with its own hints
		line += fmt.Sprintf("  [black:%s:b] %s [-:-:-]  [gray]enter log · d stop · q back[-]", a.accent, a.view().Title)
	case a.view().Hidden: // e.g. a Dynamic/CRD view reached via :jump
		line += fmt.Sprintf("  [black:%s:b] %s [-:-:-]  [gray]:jump / tab to leave[-]", a.accent, a.view().Title)
	}
	a.tabs.SetText(line)
}

func (a *App) setHeaderError(err error) {
	a.header.SetText(fmt.Sprintf("[red]error: %v[-]", err))
}

const footerHelp = "[::b]q[-] quit  [::b]?[-] help  [::b]tab[-] view  [::b]t[-] newtab  [::b][ ][-] tabs  [::b]:[-] jump  [::b]enter[-] detail  [::b]/[-] filter  [::b].[-] sort  " +
	"[::b]g[-] graph  [::b]f[-] fwd  [::b]F[-] fwd-view  [::b]l[-] logs  [::b]e[-] exec  [::b]E[-] edit  [::b]s[-] scale  " +
	"[::b]R[-] restart  [::b]u[-] undo  [::b]v[-] reveal  [::b]c[-] cordon  [::b]D[-] drain  [::b]space[-] mark  [::b]d[-] del  [::b]n[-] ns  [::b]x[-] ctx"

// logoLines is the KCLI wordmark in figlet's "ANSI Shadow" style: the solid
// blocks are the letter faces, the box-drawing glyphs their drop shadow. All
// lines are the same rune width so the banner has a clean right edge.
var logoLines = []string{
	"██╗  ██╗ ██████╗██╗     ██╗",
	"██║ ██╔╝██╔════╝██║     ██║",
	"█████╔╝ ██║     ██║     ██║",
	"██╔═██╗ ██║     ██║     ██║",
	"██║  ██╗╚██████╗███████╗██║",
	"╚═╝  ╚═╝ ╚═════╝╚══════╝╚═╝",
}

// logoBanner colours the wordmark for a dynamic-colours TextView: the faces (█)
// bright aqua/bold, the shadow glyphs dim teal — the two-tone is what makes it
// read as 3D. It switches colour tags only when the glyph class changes to keep
// the markup compact, and resets at every line end.
func logoBanner() string {
	const (
		none   = iota // outside any coloured run (e.g. spaces)
		face          // solid block, bright
		shadow        // box-drawing edge, dim
	)
	var b strings.Builder
	for _, line := range logoLines {
		class := none
		for _, r := range line {
			switch {
			case r == ' ':
				b.WriteRune(' ')
				class = none
			case r == '█':
				if class != face {
					b.WriteString("[aqua::b]")
					class = face
				}
				b.WriteRune(r)
			default: // box-drawing shadow glyph
				if class != shadow {
					b.WriteString("[teal::-]")
					class = shadow
				}
				b.WriteRune(r)
			}
		}
		b.WriteString("[-:-:-]\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
