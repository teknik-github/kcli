package ui

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/teknik-github/kcli/internal/k8s"
)

// jumpToView switches to the view named by a command-jump query. Registered
// resources resolve locally; anything else (CRDs, built-ins without an explicit
// view) falls back to a discovery lookup and opens the generic Dynamic view.
func (a *App) jumpToView(query string) {
	// A user-configured alias is expanded first, so "p" -> "pods" etc. reach the
	// registered view (or a CRD) through the normal resolution below.
	if canon, ok := a.userAliases[strings.ToLower(strings.TrimSpace(query))]; ok {
		query = canon
	}
	if i, ok := resolveView(query); ok {
		if resourceViews[i].Local {
			a.gotoForwardsView() // Port-Fwd: route through so `q` remembers the back-target
			return
		}
		a.switchView(i)
		return
	}
	a.jumpDynamic(query)
}

// jumpDynamic resolves an arbitrary resource name/alias against the cluster's
// discovery info and, on success, points the Dynamic view at it. The lookup runs
// off the UI goroutine; the view is switched on the UI goroutine.
func (a *App) jumpDynamic(query string) {
	cl := a.client
	go func() {
		gvr, namespaced, kind, err := cl.ResolveResource(query)
		a.tv.QueueUpdateDraw(func() {
			if err != nil {
				a.showMessage("jump", fmt.Sprintf("unknown resource: %q", query))
				return
			}
			a.setDynamicView(gvr, namespaced, kind)
		})
	}()
}

// setDynamicView rewrites the reserved Dynamic slot to point at gvr and
// activates it. Runs on the UI goroutine. The Fetch closure captures the GVR by
// value, so a later :jump that rewrites the slot cannot disturb an in-flight
// load (loadCurrentView also captures view.Fetch before spawning).
func (a *App) setDynamicView(gvr schema.GroupVersionResource, namespaced bool, kind string) {
	a.dynGVR = gvr
	a.dynNamespaced = namespaced
	*resourceViews[a.dynIdx] = dynamicViewDef(gvr, namespaced, kind)

	// switchView bails when the target index is already active, so re-jumping
	// from one dynamic resource to another needs an explicit reload.
	if a.viewIdx == a.dynIdx {
		a.rows = nil
		a.sortCol = -1
		a.sortDesc = false
		a.table.Clear()
		a.drawTabs()
		a.drawHeader()
		go a.refresh()
		return
	}
	a.switchView(a.dynIdx)
}

// dynamicViewDef builds a self-contained view for one GVR. It is a value, not a
// write into the shared Dynamic slot, so a restored workspace can hand each
// CRD tab its own copy (tabState.dynSlot) without them overwriting each other.
// The Fetch closure captures the GVR by value for the same reason.
func dynamicViewDef(gvr schema.GroupVersionResource, namespaced bool, kind string) viewDef {
	v := viewDef{
		ID:            "dynamic",
		Title:         kind,
		Columns:       []string{"NAMESPACE", "NAME", "AGE"},
		StatusCol:     -1,
		ClusterScoped: !namespaced,
		Hidden:        true,
		Dynamic:       true,
	}
	if !namespaced {
		v.Columns = []string{"NAME", "AGE"}
	}

	gvrCopy, nsd := gvr, namespaced
	v.Fetch = func(ctx context.Context, c *k8s.Client, ns string) ([]Row, error) {
		items, err := c.ListDynamic(ctx, gvrCopy, nsd, ns)
		if err != nil {
			return nil, err
		}
		rows := make([]Row, len(items))
		for i, it := range items {
			if nsd {
				rows[i] = Row{it.Namespace, it.Name, []string{it.Namespace, it.Name, it.Age}}
			} else {
				rows[i] = Row{"", it.Name, []string{it.Name, it.Age}}
			}
		}
		return rows, nil
	}
	return v
}
