package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/teknik-github/kcli/internal/k8s"
)

// The Pulse view is a cluster health summary: one row per resource kind with
// how many objects are healthy, how many are not, and why. It owns no listers
// of its own — it reuses the registered views' Fetch functions and classifies
// their rows, so a resource added to the registry can be summarised here by
// adding its ID to pulseKinds and nothing else.

// init wires the Pulse view's Fetch. It cannot be set in the registry literal:
// pulseRows reads resourceViews, which would make the variable depend on itself.
func init() {
	if v, ok := viewByID("pulse"); ok {
		v.Fetch = pulseRows
	}
}

// pulseKinds are the view IDs summarised, in row order.
var pulseKinds = []string{
	"pod", "deployment", "statefulset", "daemonset",
	"node", "pvc", "job", "service", "ingress", "event",
}

// okStatuses are the status-column values that count as healthy. Anything else
// in a status column is counted as a problem and named in the DETAIL column —
// including states this list simply doesn't know, which is the safe direction
// for a health screen.
var okStatuses = map[string]bool{
	"Running": true, "Succeeded": true, "Completed": true, "Bound": true,
	"Normal": true, "Ready": true, "Active": true, "Available": true, "True": true,
}

// pulseRows builds the summary. Every kind is fetched concurrently against the
// same client and context; a kind that fails reports ERR in its own row instead
// of failing the whole screen.
func pulseRows(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
	rows := make([]Row, len(pulseKinds))
	var wg sync.WaitGroup
	for i, id := range pulseKinds {
		v, ok := viewByID(id)
		if !ok || v.Fetch == nil {
			continue
		}
		wg.Add(1)
		go func(i int, v *viewDef) {
			defer wg.Done()
			vns := ns
			if v.ClusterScoped {
				vns = ""
			}
			got, err := v.Fetch(ctx, c, vns)
			rows[i] = pulseRow(v, got, err)
		}(i, v)
	}
	wg.Wait()

	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		if r.Name != "" { // skip kinds that aren't registered
			out = append(out, r)
		}
	}
	return out, nil
}

// pulseRow turns one kind's rows into a summary row. Name carries the view ID
// so Enter can jump straight to that view.
func pulseRow(v *viewDef, rows []Row, err error) Row {
	if err != nil {
		return Row{"", v.ID, []string{v.Title, "-", "-", "-", "ERR", firstLine(err.Error())}}
	}
	ok, warn, detail := pulseCount(v, rows)
	health := "OK"
	switch {
	case len(rows) == 0: // nothing to be unhealthy about
		detail = ""
	case ok == 0:
		health = "FAIL"
	case warn > 0:
		health = "WARN"
	}
	return Row{"", v.ID, []string{v.Title, itoa(len(rows)), itoa(ok), itoa(warn), health, detail}}
}

// pulseCount classifies a kind's rows. Views with a status column are judged on
// it; the rest fall back to a READY "n/m" column (workloads); anything with
// neither is informational and counts as healthy.
func pulseCount(v *viewDef, rows []Row) (ok, warn int, detail string) {
	bad := map[string]int{}
	if v.StatusCol >= 0 {
		for _, r := range rows {
			s := cellAt(r, v.StatusCol)
			if okStatuses[s] {
				ok++
				continue
			}
			warn++
			bad[strings.ToLower(s)]++
		}
		return ok, warn, topReasons(bad)
	}

	// Workloads carry their health in an "n/m" column: READY for controllers,
	// COMPLETIONS for Jobs.
	col, label := columnIndex(v, "READY"), "not ready"
	if col < 0 {
		col, label = columnIndex(v, "COMPLETIONS"), "incomplete"
	}
	if col < 0 {
		return len(rows), 0, "" // no health signal (Services, Ingresses, …)
	}
	for _, r := range rows {
		if readyComplete(cellAt(r, col)) {
			ok++
			continue
		}
		warn++
		bad[label]++
	}
	return ok, warn, topReasons(bad)
}

// readyComplete reports whether a "n/m" READY cell is fully satisfied. A "0/0"
// (scaled to zero on purpose) counts as complete.
func readyComplete(cell string) bool {
	have, want, found := strings.Cut(cell, "/")
	if !found {
		return true // not the shape we understand; don't cry wolf
	}
	return strings.TrimSpace(have) == strings.TrimSpace(want)
}

// topReasons renders the three most common problems, most frequent first —
// e.g. "2 pending, 1 crashloopbackoff".
func topReasons(bad map[string]int) string {
	if len(bad) == 0 {
		return ""
	}
	type kv struct {
		reason string
		n      int
	}
	list := make([]kv, 0, len(bad))
	for r, n := range bad {
		if r == "" {
			r = "unknown"
		}
		list = append(list, kv{r, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].reason < list[j].reason // stable output for equal counts
	})
	parts := make([]string, 0, 3)
	for i, e := range list {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("+%d more", len(list)-3))
			break
		}
		parts = append(parts, fmt.Sprintf("%d %s", e.n, e.reason))
	}
	return strings.Join(parts, ", ")
}

// firstLine trims a multi-line error down to something a table cell can hold.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 70 {
		s = s[:69] + "…"
	}
	return s
}

// columnIndex finds a view's column by title, or -1.
func columnIndex(v *viewDef, title string) int {
	for i, c := range v.Columns {
		if c == title {
			return i
		}
	}
	return -1
}

// viewByID / viewIndexByID look a registered view up by its singular kind.
func viewByID(id string) (*viewDef, bool) {
	if i := viewIndexByID(id); i >= 0 {
		return resourceViews[i], true
	}
	return nil, false
}

func viewIndexByID(id string) int {
	for i, v := range resourceViews {
		if v.ID == id {
			return i
		}
	}
	return -1
}

// gotoPulse jumps to the Pulse view (bound to '0', next to the 1..9 view keys).
func (a *App) gotoPulse() {
	if i := viewIndexByID("pulse"); i >= 0 {
		a.switchView(i)
	}
}

// jumpFromPulse opens the view for the summary row under the cursor, so Enter
// on "Pods 3 warn" lands in the Pods list.
func (a *App) jumpFromPulse() {
	row, ok := a.selectedRow()
	if !ok {
		return
	}
	if i := viewIndexByID(row.Name); i >= 0 {
		a.switchView(i)
	}
}
