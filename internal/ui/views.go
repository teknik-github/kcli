package ui

import (
	"sort"
	"strconv"
	"strings"
)

// view returns the active view definition.
func (a *App) view() *viewDef { return resourceViews[a.viewIdx] }

// match reports whether any of a row's visible cells (or its namespace/name
// keys) contain the active filter (case-insensitive). An empty filter matches
// everything. Searching every cell lets you filter by any column — e.g. Events
// by reason/object, Pods by node or status.
func (a *App) match(r Row) bool {
	if a.filter == "" {
		return true
	}
	f := strings.ToLower(a.filter)
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

// filteredRows returns the current view's rows matching the active filter and
// ordered by the active sort column (fetch order when unsorted).
func (a *App) filteredRows() []Row {
	var out []Row
	if a.filter == "" {
		if a.sortCol < 0 {
			return a.rows // no filter, no sort: hand back the original slice
		}
		out = append(out, a.rows...) // copy so sorting never mutates a.rows
	} else {
		out = make([]Row, 0, len(a.rows))
		for _, r := range a.rows {
			if a.match(r) {
				out = append(out, r)
			}
		}
	}
	a.sortRows(out)
	return out
}

// sortRows orders rows in place by the active sort column, if any.
func (a *App) sortRows(rows []Row) {
	col := a.sortCol
	if col < 0 {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		x, y := cellAt(rows[i], col), cellAt(rows[j], col)
		if x == y {
			return false // keep stable order for equal cells
		}
		if a.sortDesc {
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
