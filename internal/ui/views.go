package ui

import (
	"sort"
	"strconv"
	"strings"
)

// view returns the active view definition.
func (a *App) view() *viewDef { return resourceViews[a.viewIdx] }

// resolveView maps a command-jump query (a resource name or alias,
// case-insensitive) to a view index. It matches the view's ID, its
// "s"-pluralised ID, the lowercased Title, and any explicit Aliases — so ":svc",
// ":services", and ":Services" all reach the same view.
func resolveView(query string) (int, bool) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return -1, false
	}
	for i, v := range resourceViews {
		if v.Hidden { // the Dynamic placeholder is not a jump target by name
			continue
		}
		if q == v.ID || q == v.ID+"s" || q == strings.ToLower(v.Title) {
			return i, true
		}
		for _, al := range v.Aliases {
			if q == al {
				return i, true
			}
		}
	}
	return -1, false
}

// localRows builds a Local view's rows from App state. Local views declare
// their own builder in the registry, so nothing here needs to know whether the
// view on screen is Port-Fwd, Bench, or something added later.
func localRows(v *viewDef, a *App) []Row {
	if v.LocalRows == nil {
		return nil
	}
	return v.LocalRows(a)
}

// gotoLocalView switches to an app-local view by ID, remembering the current
// view so `q` returns to it. Jumping between two Local views keeps the original
// cluster view as the way back.
func (a *App) gotoLocalView(id string) {
	i := viewIndexByID(id)
	if i < 0 || i == a.viewIdx {
		return
	}
	if !a.view().Local {
		a.prevViewIdx = a.viewIdx
	}
	a.switchView(i)
}

// match reports whether any of a row's visible cells (or its namespace/name
// keys) contain the active filter (case-insensitive). An empty filter matches
// everything. Searching every cell lets you filter by any column — e.g. Events
// by reason/object, Pods by node or status.
func (a *App) match(r Row) bool { return rowMatches(r, strings.ToLower(a.filter)) }

// rowMatches is match with the filter already lowercased, so a parked tab's rows
// (split pane) can be filtered without the App's live fields.
func rowMatches(r Row, f string) bool {
	if f == "" {
		return true
	}
	if strings.Contains(strings.ToLower(r.Namespace), f) ||
		strings.Contains(strings.ToLower(r.Name), f) {
		return true
	}
	for _, cell := range r.Cells {
		if strings.Contains(strings.ToLower(cell), f) {
			return true
		}
	}
	return false
}

// clearFilter removes any active filter and repaints. The cursor resets to the
// top because the visible row set changes.
func (a *App) clearFilter() {
	a.filter = ""
	a.table.Select(1, 0)
	a.drawHeader()
	a.drawTable()
}

// filteredRows returns the current view's rows matching the active filter and
// ordered by the active sort column (fetch order when unsorted).
func (a *App) filteredRows() []Row {
	return filterSortRows(a.rows, a.filter, a.sortCol, a.sortDesc)
}

// filterSortRows is filteredRows over explicit state, so a parked tab shown in
// the other split pane goes through exactly the same filter and sort.
func filterSortRows(rows []Row, filter string, sortCol int, sortDesc bool) []Row {
	var out []Row
	if filter == "" {
		if sortCol < 0 {
			return rows // no filter, no sort: hand back the original slice
		}
		out = append(out, rows...) // copy so sorting never mutates the source
	} else {
		f := strings.ToLower(filter)
		out = make([]Row, 0, len(rows))
		for _, r := range rows {
			if rowMatches(r, f) {
				out = append(out, r)
			}
		}
	}
	sortRowsBy(out, sortCol, sortDesc)
	return out
}

// sortRowsBy orders rows in place by the given column, if any.
func sortRowsBy(rows []Row, col int, desc bool) {
	if col < 0 {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		x, y := cellAt(rows[i], col), cellAt(rows[j], col)
		if x == y {
			return false // keep stable order for equal cells
		}
		if desc {
			return cellLess(y, x)
		}
		return cellLess(x, y)
	})
}

// cellAt returns a row's cell at index c, or "" if out of range.
func cellAt(r Row, c int) string {
	if c < 0 || c >= len(r.Cells) {
		return ""
	}
	return r.Cells[c]
}

// cellLess compares two table cells. Since sorting only ever compares cells
// from the same column, units are homogeneous, so it can try in order:
//  1. duration ("3d" > "5m" > "45s") — fixes AGE, which mixes units
//  2. leading number ("12m" < "100m", "34Mi", restart counts)
//  3. lexical
func cellLess(x, y string) bool {
	if dx, okx := durationSeconds(x); okx {
		if dy, oky := durationSeconds(y); oky {
			if dx != dy {
				return dx < dy
			}
			return x < y
		}
	}
	nx, okx := leadingNum(x)
	ny, oky := leadingNum(y)
	if okx && oky && nx != ny {
		return nx < ny
	}
	return x < y
}

// durationSeconds parses a compact kubectl age ("45s", "5m", "2h", "3d") into
// seconds. It requires a number followed by exactly one known unit, so cells
// like "34Mi" or "12" fall through to numeric/lexical comparison.
func durationSeconds(s string) (float64, bool) {
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 || i >= len(s) {
		return 0, false // need a number AND a unit
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, false
	}
	mult, ok := map[string]float64{"s": 1, "m": 60, "h": 3600, "d": 86400}[s[i:]]
	if !ok {
		return 0, false
	}
	return num * mult, true
}

// leadingNum parses a number at the start of s (e.g. "12m" -> 12, "34Mi" -> 34).
func leadingNum(s string) (float64, bool) {
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, false
	}
	f, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// rowCount returns how many rows the current view holds after filtering.
func (a *App) rowCount() int { return len(a.filteredRows()) }

// selectedRow returns the row under the cursor, or false if the table is empty.
func (a *App) selectedRow() (Row, bool) {
	rows := a.filteredRows()
	r, _ := a.table.GetSelection()
	idx := r - 1 // account for the header row
	if idx < 0 || idx >= len(rows) {
		return Row{}, false
	}
	return rows[idx], true
}

// cycleSort advances the sort column: none -> col0 -> ... -> colN-1 -> none.
func (a *App) cycleSort() {
	n := len(a.view().Columns)
	if a.sortCol >= n-1 {
		a.sortCol = -1 // wrap back to fetch order
	} else {
		a.sortCol++
	}
	a.sortDesc = false
	a.table.Select(1, 0) // row order changed; reset cursor to the top
	a.drawHeader()
	a.drawTable()
}

// toggleSortDir flips ascending/descending (no-op while unsorted).
func (a *App) toggleSortDir() {
	if a.sortCol < 0 {
		return
	}
	a.sortDesc = !a.sortDesc
	a.table.Select(1, 0)
	a.drawHeader()
	a.drawTable()
}

// sortLabel describes the active sort for the header, or "" when unsorted.
func (a *App) sortLabel() string {
	if a.sortCol < 0 || a.sortCol >= len(a.view().Columns) {
		return ""
	}
	arrow := "↑"
	if a.sortDesc {
		arrow = "↓"
	}
	return a.view().Columns[a.sortCol] + arrow
}

// selectedName returns the kind/namespace/name of the row under the cursor.
func (a *App) selectedName() (kind, namespace, name string, ok bool) {
	row, ok := a.selectedRow()
	if !ok {
		return "", "", "", false
	}
	return a.view().ID, row.Namespace, row.Name, true
}
