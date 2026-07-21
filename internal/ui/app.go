// Package ui builds the interactive terminal interface with tview/tcell.
package ui

import (
	"context"
	"fmt"
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
	logo   *tview.TextView
	header *tview.TextView
	tabs   *tview.TextView
	table  *tview.Table
	footer *tview.TextView

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

	rows      []Row          // current view's data, in fetch order
	forwards  []*portForward // active background port-forwards
	nextFwdID int

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

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(top, len(logoLines), 0, false).
		AddItem(a.tabs, 1, 0, false).
		AddItem(a.table, 0, 1, true).
		AddItem(a.footer, 1, 0, false)

	a.pages = tview.NewPages().AddPage("main", flex, true, true)

	a.table.SetInputCapture(a.onTableKey)
	a.tv.SetRoot(a.pages, true).SetFocus(a.table)

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
	return a.tv.Run()
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
	a.tv.QueueUpdate(a.loadCurrentView)
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
	a.sortCol = -1 // sort is per-view; reset on switch
	a.sortDesc = false
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
	case a.view().Local: // e.g. Port-Fwd: show it separately with a back hint
		line += fmt.Sprintf("  [black:%s:b] %s [-:-:-]  [gray]q back[-]", a.accent, a.view().Title)
	case a.view().Hidden: // e.g. a Dynamic/CRD view reached via :jump
		line += fmt.Sprintf("  [black:%s:b] %s [-:-:-]  [gray]:jump / tab to leave[-]", a.accent, a.view().Title)
	}
	a.tabs.SetText(line)
}

func (a *App) setHeaderError(err error) {
	a.header.SetText(fmt.Sprintf("[red]error: %v[-]", err))
}

const footerHelp = "[::b]q[-] quit  [::b]?[-] help  [::b]tab[-] view  [::b]:[-] jump  [::b]enter[-] detail  [::b]/[-] filter  [::b].[-] sort  " +
	"[::b]g[-] graph  [::b]f[-] fwd  [::b]F[-] fwd-view  [::b]l[-] logs  [::b]e[-] exec  [::b]E[-] edit  [::b]s[-] scale  " +
	"[::b]R[-] restart  [::b]u[-] undo  [::b]v[-] reveal  [::b]c[-] cordon  [::b]D[-] drain  [::b]d[-] del  [::b]n[-] ns  [::b]x[-] ctx"

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
