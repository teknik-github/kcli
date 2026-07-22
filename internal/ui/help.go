package ui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// showHelp opens a scrollable reference of every key binding plus the `:jump`
// aliases for each resource. Bound to "?" from the table.
func (a *App) showHelp() {
	var b strings.Builder

	b.WriteString("[yellow::b]kcli — keys[-::-]\n\n")

	b.WriteString("[aqua::b]navigation[-::-]\n")
	writeKeys(&b, [][2]string{
		{"tab / shift-tab", "cycle views"},
		{"1..9", "jump to view 1..9"},
		{"t / w", "new tab (clone current) / close tab"},
		{"T", "rename the active tab (empty = auto label)"},
		{"[ / ]", "previous / next tab"},
		{"alt+1..9", "jump to tab 1..9"},
		{"| / -", "split the screen (side by side / stacked); again = unsplit"},
		{"\\", "move focus to the other split pane"},
		{":", "command-jump to any resource by name/alias (incl. CRDs)"},
		{"/", "filter rows (any column)"},
		{"esc", "clear the active filter"},
		{". / ,", "cycle sort column / flip direction"},
		{"n", "switch namespace"},
		{"x", "switch context (kubeconfig)"},
		{"r", "refresh now"},
		{"a", "toggle corner GIF animation (needs $KCLI_SPLASH)"},
		{"q", "quit (or leave a hidden view)"},
		{"?", "this help"},
	})

	b.WriteString("\n[aqua::b]actions (apply where the view supports them)[-::-]\n")
	writeKeys(&b, [][2]string{
		{"enter", "detail (YAML + events)"},
		{"l", "logs (follow; p toggles previous, / greps)"},
		{"L", "tail many pods at once (marked rows, else all visible)"},
		{"e", "exec shell"},
		{"E", "edit YAML in $EDITOR and apply"},
		{"g", "live CPU/MEM graph"},
		{"f", "port-forward (pods and services)"},
		{"F", "open the Port-Fwd view (enter: live log, d: stop)"},
		{"s", "scale"},
		{"R", "rollout restart"},
		{"u", "rollout undo (previous revision)"},
		{"v", "reveal secret values (decoded)"},
		{"c", "cordon / uncordon node"},
		{"D", "drain node"},
		{"space", "mark/unmark row (multi-select, Delete-capable views)"},
		{"d", "delete (marked rows if any, else the current row)"},
	})

	b.WriteString("\n[aqua::b]:jump aliases[-::-]\n")
	for _, v := range resourceViews {
		if v.Hidden {
			continue
		}
		names := append([]string{v.ID}, v.Aliases...)
		fmt.Fprintf(&b, "  [white]%-14s[-] %s\n", v.Title, strings.Join(names, ", "))
	}
	b.WriteString("\n[gray]Unlisted resources (CRDs, etc.) are reachable by name via \":\".[-]\n")
	b.WriteString("[gray]q / esc to close.[-]")

	view := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false)
	view.SetBorder(true).SetTitle(" help ")
	view.SetText(b.String())
	view.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape || e.Rune() == 'q' || e.Rune() == '?' {
			a.closeModal("help")
			return nil
		}
		return e
	})
	a.openModalFull("help", view)
}

// writeKeys appends a two-column key/description block.
func writeKeys(b *strings.Builder, rows [][2]string) {
	for _, r := range rows {
		fmt.Fprintf(b, "  [white::b]%-16s[-::-] %s\n", r[0], r[1])
	}
}
