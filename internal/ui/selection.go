package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
)

// markColor is the subtle background painted behind a marked row. The cursor's
// own (accent) selection background still paints on top of the current row, so a
// marked-and-selected row stays distinguishable.
var markColor = tcell.NewRGBColor(70, 70, 110)

// rowKey identifies a row across filter/sort/refresh by its namespace and name —
// the same identity actions use. Marks are always within the current view (they
// clear on any view/namespace/context switch), so kind is implicit.
func rowKey(r Row) string { return r.Namespace + "\x00" + r.Name }

// toggleMark flips the mark on the row under the cursor. Only offered in views
// that support Delete (the only bulk action today), so a mark always means
// something actionable.
func (a *App) toggleMark() {
	if !a.view().Caps.Delete {
		return
	}
	row, ok := a.selectedRow()
	if !ok {
		return
	}
	if a.marked == nil {
		a.marked = map[string]bool{}
	}
	k := rowKey(row)
	if a.marked[k] {
		delete(a.marked, k)
	} else {
		a.marked[k] = true
	}
	a.drawHeader()
	a.drawTable()
}

// clearMarks drops all marks. Called whenever the row identity space changes
// (view / namespace / context switch).
func (a *App) clearMarks() {
	a.marked = nil
}

// markedTargets resolves the marked keys back to namespace/name pairs.
func (a *App) markedTargets() []struct{ ns, name string } {
	out := make([]struct{ ns, name string }, 0, len(a.marked))
	for k := range a.marked {
		ns, name, _ := strings.Cut(k, "\x00")
		out = append(out, struct{ ns, name string }{ns, name})
	}
	return out
}

// confirmBulkDelete confirms, then deletes every marked resource in the current
// view, reporting how many succeeded. Runs off the UI goroutine with a pinned
// client, like every other action.
func (a *App) confirmBulkDelete() {
	kind := a.view().ID
	targets := a.markedTargets()
	cl := a.client
	a.confirm("confirm", fmt.Sprintf("Delete %d marked %s?", len(targets), kind), "Delete", func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			var failed int
			var firstErr error
			for _, t := range targets {
				if err := cl.Delete(ctx, kind, t.ns, t.name); err != nil {
					failed++
					if firstErr == nil {
						firstErr = err
					}
				}
			}
			a.tv.QueueUpdateDraw(func() {
				a.clearMarks()
				if firstErr != nil {
					a.showMessage("delete", fmt.Sprintf("deleted %d/%d, %d failed — first error: %v",
						len(targets)-failed, len(targets), failed, firstErr))
				}
			})
			a.refresh()
		}()
	})
}
