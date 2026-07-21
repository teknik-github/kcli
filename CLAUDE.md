# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`kcli` — an interactive terminal UI (TUI) for managing Kubernetes, built with `tview`/`tcell` and the official `client-go`. A lightweight k9s-style browser: multi-resource views, live metrics, logs (with grep), exec, scale, edit, delete, rollout restart/undo, cordon/drain, reveal-secret, port-forward, runtime context switch, `:`-command-jump, and a generic dynamic view that reaches any GVR/CRD.

## Build & run

Go lives at `/usr/local/go/bin` (not on `PATH` by default) — prefix commands:

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o kcli .        # build binary
go vet ./...              # vet
gofmt -w internal/ main.go
./kcli                    # run (needs a real terminal + reachable cluster)
```

Kubeconfig resolves in order: `$KUBECONFIG` → `~/.kube/config` → in-cluster. Metrics (CPU/MEM columns, graphs) need metrics-server; absent, those render `-` and never error.

## Testing a TUI

There is no permanent test suite. Two ways to verify changes:

- **Pure logic** — write a throwaway `*_test.go` (e.g. `cellLess`, `maskSecret`, `humanAge`, `toPod`), run `go test ./internal/...`, then delete it. Client listers can be smoke-tested against the live cluster (they self-skip when `NewClient` fails).
- **Rendering / interaction** — the app needs a real PTY; `script`/plain pipes don't allocate one. Drive it from Python: `pty.fork()`, `os.execvp("./kcli", ...)`, size it with `TIOCSWINSZ` (wide, e.g. 220×50, or output truncates), feed keystrokes with `os.write(fd, ...)`, read the screen back, strip ANSI. Note: `tview` positions text with cursor-move escapes, not spaces — after stripping escapes words are concatenated (assert `sort:CPU`, not `sort: CPU`). Non-ASCII glyphs (sparklines, arrows, box-drawing) survive only in the raw bytes, not after an ASCII filter.

`kubectl exec` and interactive pod exec are blocked by the environment's command classifier — the exec runtime path can't be verified here; verify it builds and is wired, and say so.

## Architecture

Three packages under `internal/`:

- **`internal/config`** — optional user config (`$KCLI_CONFIG` → `$XDG_CONFIG_HOME/kcli/config.yaml` → `~/.config/kcli/config.yaml`). Best-effort: a missing/malformed file yields defaults, never a startup error. Supplies startup namespace, refresh cadence (`baseRefresh`), accent colour, and custom `:jump` aliases. `main.go` loads it and passes it to `NewApp`.
- **`internal/k8s`** — all cluster access (`client-go`). `Client` wraps a typed `clientset`, an optional `*metricsv.Clientset` (best-effort), and the `*rest.Config` (kept for streaming subresources). Each resource has a display struct (`Pod`, `Deployment`, …) flattened for the table, plus a lister that sorts by `(namespace, name)`. `Describe`/`Delete`/`Scale` are `kind`-string dispatchers. `exec.go` and `portforward.go` hold the SPDY streaming subresources.
- **`internal/ui`** — the `tview` app. `App` (in `app.go`) owns the widget tree and mutable state.

### The view registry — the central pattern

`registry.go` is the single source of truth for what resources exist. Everything else is generic. A resource is one `*viewDef` appended to `resourceViews`:

```go
type Row struct{ Namespace, Name string; Cells []string } // uniform, display-ready
type viewDef struct {
    ID              string        // singular kind for Describe/Delete/Scale ("pod", ...)
    Aliases         []string      // extra :jump keywords ("po", "svc", "cj", ...)
    Title           string
    Columns         []string
    StatusCol       int           // column to color as a status, -1 = none
    ClusterScoped   bool          // no namespace (nodes)
    Local           bool          // rows come from App state, not the cluster (Port-Fwd)
    Hidden          bool          // omitted from the tab bar and Tab cycling (reach via :jump)
    Dynamic         bool          // generic view backed by the dynamic client (CRDs/any GVR)
    RefreshInterval time.Duration // per-view auto-refresh cadence; 0 = default (3s)
    Caps            viewCaps      // Logs/Exec/Scale/Graph/Delete/PortForward/Restart/Rollback/Cordon/Drain/Edit/Reveal
    Fetch           func(ctx, *k8s.Client, ns) ([]Row, error) // list + map (+metrics enrich)
}
```

**To add a resource: add one `viewDef` + one client lister. Nothing else.** It automatically gets a tab, `:jump` alias, filter, sort, detail, and any actions declared in `Caps`. Do NOT reintroduce per-resource `switch` statements in `app.go`/`pods.go`/`views.go` — that was the pre-refactor design and was deliberately removed.

`filteredRows()` (`views.go`) filters then sorts `a.rows` generically; `drawTable`, `selectedRow`, `rowCount`, and `selectedName` all read through it, so filter/sort/selection stay consistent. Key handling (`onTableKey` in `pods.go`) is data-driven off `Caps`, not per-view `if` checks.

### Navigation: number keys, `:`-jump, dynamic view

Number keys `1`-`9` map to `switchView(0..8)` — only the first nine non-hidden views. Everything past that (and any CRD) is reached by `:` command-jump (`showCommandDialog` → `resolveView`/`jumpToView`, `ui/dynamic.go`). `resolveView` matches a query against each view's `ID`, `ID+"s"`, lowercased `Title`, and `Aliases`. On a miss it falls back to `jumpDynamic`: `k8s.ResolveResource` maps the name through a cached discovery **RESTMapper wrapped in a ShortcutExpander** (so `po`/`pv`/`cm` short names resolve like kubectl), then `setDynamicView` rewrites the reserved **Dynamic slot** (a `Hidden`+`Dynamic` `viewDef` appended last, index `a.dynIdx`) with a GVR-specific `Fetch` closure and switches to it. The dynamic view is read-only: generic NAMESPACE/NAME/AGE columns + YAML detail via `DescribeDynamic`; `showDetail` branches on `a.view().Dynamic`. `drawTabs`/`cycleView` skip `Local || Hidden`; an active `Local`/`Hidden` view shows its own label + a leave hint (`q back` / `:jump / tab to leave`). `tab` from a hidden view lands on a normal one.

### tview concurrency invariant (critical)

`QueueUpdateDraw` **blocks** until the event loop drains it, and that loop only runs inside `tv.Run()`. So the first refresh must never be called synchronously before `Run()` — it deadlocks and the screen shows nothing (only the statically-set footer). `autoRefresh` runs in its own goroutine for this reason. All background work (refresh, metrics, graph sampling, port-forward status, log/describe fetches) mutates UI state only inside a `QueueUpdateDraw` closure — that closure is the only place it's safe to touch widgets/state, so no locks are used.

**Shared state is read on the UI goroutine, never on a background one.** `refresh()` is just `QueueUpdate(a.loadCurrentView)`; `loadCurrentView` (on the UI goroutine) reads `viewIdx`/`client`/`namespace`/`view.Fetch`, then spawns the cluster fetch with those captured as locals and stores the result back via `QueueUpdateDraw` (guarded by `viewIdx != idx || clientGen != gen` to drop stale loads). This is what makes runtime **context switch** (`x`, which reassigns `a.client` on the UI goroutine) race-free.

**Every action that spawns a goroutine must capture `cl := a.client` on the UI goroutine first** and use `cl` inside — never read `a.client` from a background goroutine. Besides being race-free, this pins the action to the cluster it was started on, so a mid-flight context switch can't redirect it. `edit`/`logs`/`detail`/`scale`/`delete`/`restart`/`rollback`/`cordon`/`drain`/`graph`/`exec`/`port-forward`/`reveal`/`pickContainer` all follow this rule.

The one field the `autoRefresh` ticker goroutine reads is `refreshEvery` (`atomic.Int64`, per-view cadence in ns), published by `switchView` via `publishCadence()`. It is atomic precisely because it crosses goroutines; nothing else does.

`k8s.Client` caches its discovery `RESTMapper` and `dynamic.Interface` lazily (`dynamic.go`); a fresh `Client` per context (`WithContext`) starts those caches empty, so no stale mapper survives a switch.

### Long-running actions

- **Modals** live on a `tview.Pages` overlay: `openModal` (centered box), `openModalFull` (full screen). `closeModal` removes the page and refocuses the table. `confirm(page, msg, okLabel, onYes)` is the shared Cancel/OK helper (restart/rollback/cordon/drain/reveal use it).
- **help** (`ui/help.go`, `?`) is a full-screen overlay listing every binding plus the `:jump` aliases generated from `resourceViews`.
- **exec** (`ui/exec.go`) uses `tv.Suspend` to hand the real terminal to an interactive shell, then resumes. `pickContainer` prompts on multi-container pods (init containers listed first); single-container skips the picker.
- **logs** (`ui/modals.go`) stream into a `logState` buffer (`lines`/`partial`/`grep`, all touched only on the UI goroutine). `p` toggles follow vs previous-container (re-streams); `/` opens a grep prompt that re-filters the *same* buffer in place via `logState.render()` (no re-stream). Buffer capped at `logMaxLines`.
- **graph** (`ui/graph.go`) samples metrics on its own ticker into a ring buffer and renders an `asciigraph` line chart (CPU red / MEM blue via `asciigraph.SeriesColors` → `tview.TranslateANSI`; raw ANSI would otherwise be swallowed by the TextView). Sampler goroutine is stopped via `graphStop` on close.
- **edit** (`ui/edit.go`, `E`): `GetYAML` (fresh copy with resourceVersion, no events, secrets **unmasked** so values round-trip) → `$EDITOR` under `Suspend` → `ApplyYAML` (dynamic-client Update, i.e. `kubectl edit` semantics). The same captured `cl` fetches and applies.
- **rollout undo** (`k8s/rollout.go`, `u`, `Caps.Rollback`): no server-side endpoint exists, so it is reconstructed client-side — Deployments swap in the prior ReplicaSet's pod template (stripping `pod-template-hash`); StatefulSets/DaemonSets re-apply the previous `ControllerRevision` as a strategic-merge patch. Returns a message; errors gracefully with "no previous revision" when there is only one.
- **reveal secret** (`u/modals.go`, `v`, `Caps.Reveal`): confirms first, then `SecretData` decodes and a full-screen pane shows plain-text key/values. Distinct from `Describe`, which always masks.
- **port-forward** (`ui/portforward.go`, `f`) runs in the background, tracked in `App.forwards` and surfaced as `⇄ N` in the header. Works on **Pods and Services** — a Service (`Caps.PortForward`) resolves to a Ready backing pod via `ServiceForwardTarget` before forwarding. The **Port-Fwd view** is a `viewDef` with `Local: true` (rows come from `App.forwards`, not the cluster; `loadCurrentView` special-cases `Local` and never calls `Fetch`). `F` jumps to it (remembering `prevViewIdx`); Enter/`d` stops the selected forward (keyed by the ID column, which survives filter/sort); `q` returns via `backView`. This is the model for any app-local (non-cluster) view.

### Secrets

`Secrets` lister carries only metadata. `Describe` masks values via `maskSecret` before rendering: markers go into `StringData` (not `Data`) because YAML base64-encodes `[]byte` `Data` — the `last-applied-configuration` annotation is stripped too.

### Cell sorting

`cellLess` (`views.go`) compares same-column cells: duration-aware first (`"5m" < "3d"`, fixing AGE), then leading-number numeric (`"12m" < "100m"`, CPU/MEM/restarts), then lexical. Safe because sorting only ever compares cells from one column, where units are homogeneous.
