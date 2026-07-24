# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`kcli` — an interactive terminal UI (TUI) for managing Kubernetes, built with `tview`/`tcell` and the official `client-go`. A lightweight k9s-style browser: multi-resource views, live metrics, logs (with grep), exec, scale, edit, delete, rollout restart/undo, cordon/drain, reveal-secret, port-forward, HTTP benchmarking, a startup update check with in-app self-update, runtime context switch, `:`-command-jump, and a generic dynamic view that reaches any GVR/CRD.

## Build & run

Go lives at `/usr/local/go/bin` (not on `PATH` by default) — prefix commands:

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o kcli .        # build binary
go vet ./...              # vet
gofmt -w internal/ main.go
./kcli                    # run (needs a real terminal + reachable cluster)
```

`main.go` silences client-go's async logging before the UI starts (`klog.SetLogger(logr.Discard())` **and** an empty `utilruntime.ErrorHandlers`): a resetting port-forward stream or a dropped watch otherwise reaches `runtime.HandleError`, which klog prints to **stderr** — and stderr scribbles straight over the full-screen TUI. klog's own `LogToStderr(false)`/`SetOutput` do not reliably mute error severity, hence the two-pronged kill. Kubeconfig resolves in order: `$KUBECONFIG` → `~/.kube/config` → in-cluster. That resolution is left entirely to `clientcmd.NewDefaultClientConfigLoadingRules()` (so a multi-file `$KUBECONFIG` merges like kubectl's); `NewClient("")` is the normal call, and its path argument only exists to set `ExplicitPath`, which overrides everything. Metrics (CPU/MEM columns, graphs) need metrics-server; absent, those render `-` and never error.

## Testing a TUI

There is no permanent test suite. Two ways to verify changes:

- **Pure logic** — write a throwaway `*_test.go` (e.g. `cellLess`, `maskSecret`, `humanAge`, `toPod`), run `go test ./internal/...`, then delete it. Client listers can be smoke-tested against the live cluster (they self-skip when `NewClient` fails).
- **Rendering / interaction** — the app needs a real PTY; `script`/plain pipes don't allocate one. Drive it from Python: `pty.fork()`, `os.execvp("./kcli", ...)`, size it with `TIOCSWINSZ` (wide, e.g. 220×50, or output truncates), feed keystrokes with `os.write(fd, ...)`, read the screen back, strip ANSI. Note: `tview` positions text with cursor-move escapes, not spaces — after stripping escapes words are concatenated (assert `sort:CPU`, not `sort: CPU`). Non-ASCII glyphs (sparklines, arrows, box-drawing) survive only in the raw bytes, not after an ASCII filter.

`kubectl exec` and interactive pod exec are blocked by the environment's command classifier — the exec runtime path can't be verified here; verify it builds and is wired, and say so.

## Architecture

Five packages under `internal/`:

- **`internal/version`** — dependency-free build/version reporting. `Current()` prefers an `-ldflags` stamp, then the `go install module@tag` build info (`runtime/debug.ReadBuildInfo`), then `(devel)`. `Latest(ctx)` queries `https://proxy.golang.org/<module>/@latest`; `IsNewer` compares clean `vX.Y.Z` tags only (build metadata after `+` ignored, anything with a `-` prerelease/pseudo suffix rejected) so a local/pseudo build is never nagged. `main.go` handles `--version`; the UI runs `checkForUpdate` at startup (see below).
- **`internal/config`** — optional user config (`$KCLI_CONFIG` → `$XDG_CONFIG_HOME/kcli/config.yaml` → `~/.config/kcli/config.yaml`). Best-effort: a missing/malformed file yields defaults, never a startup error. Supplies startup namespace, refresh cadence (`baseRefresh`), accent colour, and custom `:jump` aliases. `main.go` loads it and passes it to `NewApp`.
- **`internal/k8s`** — all cluster access (`client-go`). `Client` wraps a typed `clientset`, an optional `*metricsv.Clientset` (best-effort), the `*rest.Config` (kept for streaming subresources), and a lazily-built cached `RESTMapper`/`dynamic.Interface` plus a shared informer cache. Each resource has a display struct (`Pod`, `Deployment`, …) flattened for the table by a `toX` helper, plus a lister that reads from the informer cache and sorts by `(namespace, name)`. `Describe`/`Delete`/`Scale`/`RolloutRestart`/`RolloutUndo` are `kind`-string dispatchers. `exec.go` and `portforward.go` hold the SPDY streaming subresources.

### Informer cache & live updates (`internal/k8s/informer.go`)

Listers read from a `SharedInformerFactory` cache instead of a live List each time: `cachedObjects(ctx, gvr, ns)` starts and syncs the resource's informer on first use, then serves subsequent reads from memory — so the auto-refresh poll costs no List API calls once warm. If an informer can't sync within the fetch ctx, `cachedObjects` returns `ok=false` and the lister **falls back to a live List**, so an un-watchable resource (or RBAC gap) still works exactly as before. Each started informer registers an Add/Update/Delete handler that calls `Client.onChange`; the UI sets that callback (`SetOnChange`) to nudge a bounded `watchTrigger` channel, and `watchLoop` debounces the nudges (400ms) into a `refresh()` — this is what makes changes appear live rather than on the next tick. `Client.Stop()` tears the informers down; `switchContext` calls it on the old client before swapping in the new one (and re-registers `onChange`). Metrics are not watchable, so the poll still runs (now cache-served for the resource columns) to refresh CPU/MEM. The **Dynamic/CRD** and **Local** views do not use this path.
- **`internal/bench`** — a dependency-free HTTP load generator (`Run(ctx, Options, progress) (*Result, error)`). It knows nothing about Kubernetes; the UI hands it a plain URL. Workers tally into per-worker shards (no locks on the hot path), merged and sorted once at the end into counts / RPS / percentiles / status codes / grouped transport errors / `Histogram(n)`. Cancelling the ctx ends the run and still returns what was measured, so a stopped benchmark still has a report.
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
    Local           bool          // rows come from App state, not the cluster (Port-Fwd, Bench)
    Hidden          bool          // omitted from the tab bar and Tab cycling (reach via :jump)
    Dynamic         bool          // generic view backed by the dynamic client (CRDs/any GVR)
    RefreshInterval time.Duration // per-view auto-refresh cadence; 0 = default (3s)
    Caps            viewCaps      // Logs/Exec/Scale/Graph/Delete/PortForward/Restart/Rollback/Cordon/Drain/Edit/Reveal/Bench
    Fetch           func(ctx, *k8s.Client, ns) ([]Row, error) // list + map (+metrics enrich)

    LocalRows func(a *App) []Row // Local: rows from App state (never Fetch)
    LocalHint string             // Local: key hints beside the tab-bar label
    OnEnter   func(a *App)       // Local: Enter action
    OnDelete  func(a *App)       // Local: `d` action
}
```

**To add a resource: add one `viewDef` + one client lister. Nothing else.** It automatically gets a tab, `:jump` alias, filter, sort, detail, and any actions declared in `Caps`. Do NOT reintroduce per-resource `switch` statements in `app.go`/`pods.go`/`views.go` — that was the pre-refactor design and was deliberately removed.

`filteredRows()` (`views.go`) filters then sorts `a.rows` generically; `drawTable`, `selectedRow`, `rowCount`, and `selectedName` all read through it, so filter/sort/selection stay consistent. Key handling (`onTableKey` in `pods.go`) is data-driven off `Caps`, not per-view `if` checks.

### Navigation: number keys, `:`-jump, dynamic view

Number keys `1`-`9` map to `switchView(0..8)` — only the first nine non-hidden views. Everything past that (and any CRD) is reached by `:` command-jump (`showCommandDialog` → `resolveView`/`jumpToView`, `ui/dynamic.go`). `resolveView` matches a query against each view's `ID`, `ID+"s"`, lowercased `Title`, and `Aliases`. On a miss it falls back to `jumpDynamic`: `k8s.ResolveResource` maps the name through a cached discovery **RESTMapper wrapped in a ShortcutExpander** (so `po`/`pv`/`cm` short names resolve like kubectl), then `setDynamicView` rewrites the reserved **Dynamic slot** (a `Hidden`+`Dynamic` `viewDef` appended last, index `a.dynIdx`) with a GVR-specific `Fetch` closure and switches to it. The dynamic view is read-only: generic NAMESPACE/NAME/AGE columns + YAML detail via `DescribeDynamic`; `showDetail` branches on `a.view().Dynamic`. `drawTabs`/`cycleView` skip `Local || Hidden`; an active `Local`/`Hidden` view shows its own label + a leave hint (`q back` / `:jump / tab to leave`). `tab` from a hidden view lands on a normal one.

### Tabs — browser-style view sessions (`ui/tabs.go`)

Multiple independent sessions kept open at once, one visible: `t` new (clones current view+namespace, fresh filter/sort/marks), `w` close (always keeps ≥1), `[`/`]` prev/next, `alt+1..9` jump to tab N, `T` rename (`tabState.name` override; empty reverts to the auto title). `renameTab` uses a bare `InputField` as the modal primitive, **not** a `tview.Form` — a Form intercepts Enter for field navigation and would leave the modal open. A `tabState` holds one session's `viewIdx`/`prevViewIdx`/`namespace`/`filter`/`sortCol`/`sortDesc`/`marked`/`rows`/`selRow` (+ dynamic-view snapshot). **The active tab's session IS the live `App` fields** — so every existing read of `a.viewIdx`/`a.namespace`/… is untouched; only tab switches move state. `saveTab` snapshots the live fields into `a.tabList[a.activeTab]`; `loadTab` restores another tab and repaints, then `go a.refresh()` reconciles any staleness (a context switch on one tab reassigns the shared `a.client` for all — `clientGen` drops stale loads). **The Dynamic/CRD slot is a single shared `viewDef`**, so a tab parked on a CRD carries its own `dynSlot` copy which `loadTab` writes back into `resourceViews[a.dynIdx]` before rendering (else another tab's CRD target would leak in). `drawTabbar` renders the workspace strip and `flex.ResizeItem`s its row to 0 with one tab (no chrome for single-tab users), 1 otherwise. All tab ops run on the UI goroutine.

### Workspaces — persisted layouts (`ui/workspace.go`, `config/workspace.go`)

`:ws save|load|rm|list [name]` (parsed in `showCommandDialog` before `jumpToView`, so `ws` never collides with a resource name). Only the **shape** of a session is stored — resource ID, namespace, filter, sort, tab name, split arrangement — never rows; a restored tab reloads from the cluster like any other. `workspaces.yaml` sits beside `config.yaml` (`config.WorkspacePath`) and is **the only file kcli writes**; loading is best-effort like the config (missing/malformed = no workspaces), while saving returns its error because the user asked for it.

`default` is restored in `NewApp` via `restoreStartupWorkspace` — safe there because it touches widgets directly and never calls `QueueUpdateDraw` (which would deadlock before `Run`). Quitting goes through `a.quit()`, which snapshots into the `last` slot before `tv.Stop()`.

`applyWorkspace` is defensive on purpose: a tab whose view ID is gone from the registry is dropped, a workspace with no usable tabs is refused (the current session stays), and a stored split is only honoured when its mode is a known one and its `paneTabs` still name 2–4 distinct existing tabs (`snapshotWorkspace` writes `paneTabs[:paneCount]`, and nothing at all while unsplit). A CRD tab stores its GVR and is rebuilt with `dynamicViewDef` (extracted from `setDynamicView`, which now writes that value into the shared Dynamic slot) — so restoring costs no discovery round-trip and each CRD tab still gets its own `dynSlot` copy.

### Pulse — cluster health summary (`ui/pulse.go`)

`0` (or `:pulse`/`:health`) opens a one-row-per-kind health table: RESOURCE / TOTAL / OK / WARN / HEALTH / DETAIL. It **owns no listers** — `pulseRows` fans out over the registered views named in `pulseKinds`, calling each one's own `Fetch` concurrently with the same client/ctx, and classifies the rows it gets back. Summarising a newly added resource is one string in `pulseKinds`.

Classification is generic, in priority order: the view's `StatusCol` (healthy = `okStatuses`), else an `n/m` `READY` column, else `COMPLETIONS` (Jobs), else "no health signal" (Services, Ingresses) which counts everything as OK. `DETAIL` is the top three problems with counts. A kind whose Fetch errors renders `ERR` in its own row instead of failing the whole view.

Two wiring details: the viewDef is appended **after** the numbered views so it does not shift anyone's `1`-`9` keys (it answers to `0`, handled just after the digit jump in `onTableKey`), and its `Fetch` is assigned in an `init()` in pulse.go — naming `pulseRows` in the registry literal is an **initialization cycle**, since `pulseRows` reads `resourceViews`. The `Pulse` flag on `viewDef` only routes Enter (`jumpFromPulse` switches to the kind under the cursor; the row's `Name` carries the view ID).

### Split view — 2 to 4 panes over the tabs (`ui/split.go`)

`|` columns, `-` rows, `+` grid (two per row, i.e. 2×2 at four panes), `\` moves focus to the next pane, `_` closes the focused pane. `|`/`-` open two panes and each further press of the same key calls `addPane` (up to `maxPanes` = 4) before finally unsplitting; `+` opens a full quad from unsplit and, when a split is already up, only changes the *arrangement* — pane count is preserved on an arrangement switch. Split is a **layout over `tabList`, not a second session model**: each pane shows one tab, and the focused pane's tab is the live `App` fields exactly as before.

The trick that keeps it cheap: **`a.table` always renders the live tab and the `a.parked` tables (maxPanes-1 of them) render the others** — focusing another pane does not move state between widgets, it **swaps their positions inside `a.body`** (`rebuildBody`), so the tab under the cursor stays put on screen while every existing `a.table` read (`selectedRow`, `drawTable`, modals refocusing it) keeps meaning "the live pane". `paneTabs[p]` is the tab at position p (0 = leftmost/topmost, filling in reading order), `paneCount` how many positions exist, `activePane` the position `a.table` sits at, and `paneTable[p]` the widget at p (`assignPaneTables`). `focusNextPane` **must** call `rebuildBody` after `loadTab`, or the tabs visibly trade places instead of the focus moving.

`paneWidget(p)` is what the layout places at position p: the pinned overlay at `overlayPane`, `a.table` at `activePane`, else a parked table. `a.table` remains "the live table" for `selectedRow`, `drawTable` and the refresh loop, so an overlay changes what is *shown* and nothing else — and because the overlay is a separate position, the focused pane can be a real table while the graph sits in another. `focusTarget()` decides which widget holds the keyboard focus (the overlay only while its pane is the focused one). `fixPanes` reconciles pane assignment after any `tabList`/`activeTab` change (called from `loadTab`) and returns whether the pane *order* moved — the only case needing a rebuild. It guarantees the invariant every drawer relies on: **`paneCount` distinct, in-range tabs, the live one at `activePane`** — activating a tab already on screen moves the focus to its pane instead of dragging content across, and too few tabs sheds panes (down to one tab: unsplit). `closeTab` fixes up `paneTabs` indices itself (removal shifts every later index; `-1` means "gone, re-pick"). `addPane` fills a new position with a tab that is not on screen, cloning the current session (`cloneTab`, extracted from `newTab` so it appends *without* activating) when they are all visible.

Parked panes are refreshed by `loadSplitPanes` (per pane: `loadPane`), called from `loadCurrentView`, following the same rules as the live fetch (view/ns/client captured on the UI goroutine, store back inside `QueueUpdateDraw`, dropped if the pane moved or `clientGen` changed). They render through the same generic helpers — `filterSortRows` (extracted from `filteredRows`) and `drawRows` (extracted from `drawTable`) — so a parked tab's filter/sort/marks look identical to a live one. A parked tab on a CRD is fetched via its own `dynSlot.Fetch` (`tabView`), never the shared Dynamic slot.

Anything that changes what a tab *shows* (view switch, namespace change, rename) must call `drawTabChrome()` — the tab label lives in two places now (workspace strip + pane border), and forgetting it leaves a stale pane title.

### tview concurrency invariant (critical)

`QueueUpdateDraw` **blocks** until the event loop drains it, and that loop only runs inside `tv.Run()`. So the first refresh must never be called synchronously before `Run()` — it deadlocks and the screen shows nothing (only the statically-set footer). `autoRefresh` runs in its own goroutine for this reason. All background work (refresh, metrics, graph sampling, port-forward status, log/describe fetches) mutates UI state only inside a `QueueUpdateDraw` closure — that closure is the only place it's safe to touch widgets/state, so no locks are used.

**Shared state is read on the UI goroutine, never on a background one.** `refresh()` is just `QueueUpdate(a.loadCurrentView)`; `loadCurrentView` (on the UI goroutine) reads `viewIdx`/`client`/`namespace`/`view.Fetch`, then spawns the cluster fetch with those captured as locals and stores the result back via `QueueUpdateDraw` (guarded by `viewIdx != idx || clientGen != gen` to drop stale loads). This is what makes runtime **context switch** (`x`, which reassigns `a.client` on the UI goroutine) race-free.

**Every action that spawns a goroutine must capture `cl := a.client` on the UI goroutine first** and use `cl` inside — never read `a.client` from a background goroutine. Besides being race-free, this pins the action to the cluster it was started on, so a mid-flight context switch can't redirect it. `edit`/`logs`/`detail`/`scale`/`delete`/`restart`/`rollback`/`cordon`/`drain`/`graph`/`exec`/`port-forward`/`reveal`/`pickContainer` all follow this rule.

The one field the `autoRefresh` ticker goroutine reads is `refreshEvery` (`atomic.Int64`, per-view cadence in ns), published by `switchView` via `publishCadence()`. It is atomic precisely because it crosses goroutines; nothing else does.

`k8s.Client` caches its discovery `RESTMapper` and `dynamic.Interface` lazily (`dynamic.go`); a fresh `Client` per context (`WithContext`) starts those caches empty, so no stale mapper survives a switch.

### Long-running actions

- **Modals** live on a `tview.Pages` overlay: `openModal` (centered box), `openModalFull` (full screen). `closeModal` removes the page and refocuses the table. A third kind sits *in the layout* rather than over it — `openPaneOverlay`/`closePaneOverlay` (`ui/split.go`) put a primitive in the live pane's slot, keeping the header, tab bars, footer and the other split panes on screen; the graph uses it. `confirm(page, msg, okLabel, onYes)` is the shared Cancel/OK helper (restart/rollback/cordon/drain/reveal use it).
- **help** (`ui/help.go`, `?`) is a full-screen overlay listing every binding plus the `:jump` aliases generated from `resourceViews`.
- **exec** (`ui/exec.go`) uses `tv.Suspend` to hand the real terminal to an interactive shell, then resumes. `pickContainer` prompts on multi-container pods (init containers listed first); single-container skips the picker.
- **logs** (`ui/modals.go`) stream into a `logState` buffer (`lines`/`partial`/`grep`, all touched only on the UI goroutine). `p` toggles follow vs previous-container (re-streams); `/` opens a grep prompt that re-filters the *same* buffer in place via `logState.render()` (no re-stream). Buffer capped at `logMaxLines`.
- **multi-pod tail** (`ui/multilog.go`, `L`, `Caps.Logs`): one pane following many pods at once. Targets come from `tailTargets()` — the marked rows if any, else all of `filteredRows()` — capped at `maxTailPods` (20 open streams; the title reports `N of M`). `startMultiTail` spawns one goroutine per pod under a **single** `context.WithCancel` stored in `a.logsCancel`, so the existing `stopLogs()` tears all of them down at once. Each goroutine resolves its container with `PodMainContainer` (first regular container, `kubectl logs` semantics — no per-pod picker is possible) and **splits lines itself**: `partial` is goroutine-local, so only whole lines cross to the UI goroutine via `pushTailLines`, and `QueueUpdateDraw` blocking is what stops a chatty pod from starving the others. The buffer is `[]mline{pod,text,err}`, not `[]string`, so `render()` can colour the pod prefix (`podPalette`, by on-screen order) and grep against pod + line. `render()` escapes only the log text — the prefix tags are emitted deliberately, so do not wrap the whole line in `tview.Escape`. Labels are bare names, or `ns/name` when the tail spans namespaces.
- **graph** (`ui/graph.go`) samples metrics on its own ticker into a ring buffer and renders an `asciigraph` line chart (CPU red / MEM blue via `asciigraph.SeriesColors` → `tview.TranslateANSI`; raw ANSI would otherwise be swallowed by the TextView). Sampler goroutine is stopped via `graphStop` on close. It opens through `openPaneOverlay`, **not** as a full-screen modal: the chart is a monitor, so it takes only the focused pane (the whole table area when unsplit) and the other panes keep refreshing beside it. The overlay is **pinned to a pane position** (`a.overlayPane`), decoupled from `a.activePane`: its own `InputCapture` sends `\` to `focusNextPane`, so the focus moves to the next pane *with the chart left in place* (`focusTarget()` then aims keys at that pane's table, `tintOverlayBorder` greys the chart's border) — that is how you can drive a neighbouring list while watching the graph. `q`/`esc` closes it (which also calls `stopGraph`, since the graph is the only overlay); any other command key closes it and re-dispatches to `onTableKey`. Only the mutators that change the **pane count or arrangement** — `toggleSplit`, `dropPane`, `closeTab`, `applyWorkspace` — call `closePaneOverlay` first, so `overlayPane` is only read while it names a live pane. `switchView` and the plain tab switches (`nextTab`/`prevTab`/`gotoTab`/`newTab`) deliberately **do not**: they only touch the focused pane, so a graph pinned to a *different* pane keeps running and keeps its history — that is what makes it consistent while you page around the other panes. (Guarding those was the bug: it reset the chart on any page switch.) When the focused pane *is* the graph's, the key arrives through the graph's own `InputCapture`, which closes the overlay before dispatching, so switching that pane's own view still closes it. `switchContext` closes it because the sampled pod belongs to the old cluster. `loadTab` re-aims the focus through `focusTarget()` whenever an overlay is up, since a tab switch may not trigger a rebuild.
- **edit** (`ui/edit.go`, `E`): `GetYAML` (fresh copy with resourceVersion, no events, secrets **unmasked** so values round-trip) → `$EDITOR` under `Suspend` → `ApplyYAML` (dynamic-client Update, i.e. `kubectl edit` semantics). The same captured `cl` fetches and applies.
- **rollout undo** (`k8s/rollout.go`, `u`, `Caps.Rollback`): no server-side endpoint exists, so it is reconstructed client-side — Deployments swap in the prior ReplicaSet's pod template (stripping `pod-template-hash`); StatefulSets/DaemonSets re-apply the previous `ControllerRevision` as a strategic-merge patch. Returns a message; errors gracefully with "no previous revision" when there is only one.
- **reveal secret** (`u/modals.go`, `v`, `Caps.Reveal`): confirms first, then `SecretData` decodes and a full-screen pane shows plain-text key/values. Distinct from `Describe`, which always masks.
- **multi-select** (`ui/selection.go`, `Space`): `a.marked` is a `rowKey`-set (`namespace\x00name`) painted with `markColor` in `drawTable`. Offered only in `Caps.Delete` views; `d` routes to `confirmBulkDelete` when any row is marked, else the single-row path. Marks clear on view/namespace/context switch (`clearMarks`). This is the pattern for future bulk actions (label, annotate).
- **HTTP benchmark** (`ui/bench.go`, `b`, `Caps.Bench`; view `B`) — see its own section below.
- **port-forward** (`ui/portforward.go`, `f`) runs in the background, tracked in `App.forwards` and surfaced as `⇄ N` in the header. Works on **Pods and Services** — a Service (`Caps.PortForward`) resolves to a Ready backing pod via `ServiceForwardTarget` before forwarding. The **Port-Fwd view** is a `viewDef` with `Local: true` (rows come from `App.forwards`, not the cluster; `loadCurrentView` special-cases `Local` and never calls `Fetch`). `F` jumps to it (remembering `prevViewIdx`); Enter/`d` stops the selected forward (keyed by the ID column, which survives filter/sort); `q` returns via `backView`. **App-local views are registry-driven**: `Local: true` plus `LocalRows`/`LocalHint`/`OnEnter`/`OnDelete`, all assigned in an `init()` in the feature's own file — naming them in the registry literal is an initialization cycle (those closures reach `resourceViews` through `App`, same trap as `pulseRows`). `loadCurrentView`, `loadPane`, `drawTabs`, and `onTableKey`'s Enter/`d` branches read those fields, so **nothing outside the registry switches on which Local view is on screen** — that is what let Bench be added beside Port-Fwd. `gotoLocalView(id)` is the shared "jump here, remember where I came from" helper behind `F` and `B`.

### HTTP benchmark (`ui/bench.go`, `internal/bench`)

`b` load-tests the selected Pod/Service/Ingress; `B` opens the **Bench** view (a `Local` view over `App.benches`, one row per run: ID/TARGET/REQS/CONC/RPS/P95/OK/ERR/STATUS). Enter opens the full report (throughput, bytes, min/mean/p50/p90/p95/p99/max, status-code table, grouped errors, latency histogram); `d` cancels a running test and drops a finished one.

The split of responsibility is the point: **`internal/bench` is the engine and has no k8s import**, `ui/bench.go` only decides *what URL* to hand it. `benchTarget` does that:

- **Pod/Service** — opens an **ephemeral port-forward** on a kernel-assigned free port (`freeLocalPort`), waits for `readyCh` (15s cap, also watching the run's ctx and the forwarder's error channel), and returns `http://127.0.0.1:<port><path>` plus a `stop` func that `runBench` defers. These forwards are deliberately **not** in `App.forwards` — they live and die with the run and must not show up in the Port-Fwd view. A Service goes through `ServiceForward` first, so `targetPort` (including named ports) is followed exactly as a manual forward would.
- **Ingress** — no forward at all: `k8s.IngressTarget` returns the published LB address (falling back to the rule's host), the `Host` header to send, and whether TLS covers it. That path measures the ingress controller too, which is the whole point of benchmarking an Ingress. `Insecure` is set for https because cluster certs are routinely self-signed.

`runBench` is a plain background goroutine following the usual rule — `cl := a.client` captured on the UI goroutine in `startBench` — and every status/progress write goes through `QueueUpdateDraw`, so `benchRun`'s fields need no locking. The engine's `progress` callback fires every 250ms and calls `redrawBench`, which repaints only when the Bench view is actually on screen.

### Update check & self-update (`ui/update.go`)

`Run()` spawns `checkForUpdate` once at startup: it calls `version.Latest` (5s ctx) and, **only if** `version.IsNewer(latest, Current())`, stores `a.latestVersion` via `QueueUpdateDraw` + `drawHeader`. `drawHeader` then adds a yellow `Update: ↑ vX.Y.Z available (:update)` line — its mere presence means "newer release exists", so the check must never set the field otherwise (it doesn't: a `(devel)`/pseudo/dirty build fails `IsNewer` and stays quiet). Best-effort: any proxy/network failure is swallowed.

`:update` (parsed in `showCommandDialog`, before `jumpToView`, like `:ws`) runs `updateCommand`: it `exec.LookPath("go")`s — no toolchain, show the manual `go install …@latest` line — then `confirm`s and runs `go install <version.Module>@latest` under `tv.Suspend` (`runUpdate`, same real-terminal-handoff pattern as `exec.go`), waiting for Enter before restoring the TUI. The running process stays the old build until the user restarts; the message says so. `go install` execution can't be verified in this environment (the command classifier blocks it) — it builds and is wired the same way as exec.

### Secrets

`Secrets` lister carries only metadata. `Describe` masks values via `maskSecret` before rendering: markers go into `StringData` (not `Data`) because YAML base64-encodes `[]byte` `Data` — the `last-applied-configuration` annotation is stripped too.

### Cell sorting

`cellLess` (`views.go`) compares same-column cells: duration-aware first (`"5m" < "3d"`, fixing AGE), then leading-number numeric (`"12m" < "100m"`, CPU/MEM/restarts), then lexical. Safe because sorting only ever compares cells from one column, where units are homogeneous.
