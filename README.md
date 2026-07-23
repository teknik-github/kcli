# kcli

```
██╗  ██╗ ██████╗██╗     ██╗
██║ ██╔╝██╔════╝██║     ██║
█████╔╝ ██║     ██║     ██║
██╔═██╗ ██║     ██║     ██║
██║  ██╗╚██████╗███████╗██║
╚═╝  ╚═╝ ╚═════╝╚══════╝╚═╝
```

**kcli** is a lightweight terminal UI (TUI) for browsing and managing Kubernetes clusters — [k9s]-style, built with [`tview`]/[`tcell`] and the official `client-go`.

A single binary with no runtime dependencies. It shows many resource kinds in tabs, with live metrics (CPU/MEM), streaming logs, exec shell, scale, rollout restart/undo, port-forward, and more.

---

## Features

- **14 resource views**: Pods, Deployments, DaemonSets, Services, Nodes, StatefulSets, ReplicaSets, PVCs, Ingresses, Jobs, CronJobs, ConfigMaps, Secrets, Events — plus a built-in **Port-Forward** view and a **Pulse** health summary.
- **Pulse** (`0`, or `:pulse`): one-screen cluster health — per kind, how many objects exist, how many are healthy, how many are not, and why (`3 pending, 1 crashloopbackoff`). `Enter` on a row opens that kind's view. Pairs well with a split pane as a permanent monitor.
- **Command-jump** (`:`): jump to any view by name or short alias (`:svc`, `:cj`, `:ev`, …).
- **Generic / CRD view**: `:` any resource the cluster knows — CustomResourceDefinitions and built-ins without a dedicated view — resolved through discovery (kubectl-style short names) and listed read-only (name/age + YAML detail).
- **Live updates** via shared informers: changes appear as they happen (watch-driven), and warm listers read from the in-memory cache instead of re-Listing the API each tick. A resource that can't be watched falls back to polling automatically.
- **Auto-refresh** every 3 seconds (per-view cadence; Events poll slower) — mainly a metrics refresh and a safety net now that resource changes are watch-driven.
- **Live metrics** (CPU/MEM columns + graphs) via metrics-server — *best-effort*; if metrics-server is absent it never errors, the columns just render `-`.
- **Log streaming (follow)** with a toggle to a crashed container's *previous* logs, and an in-pane **grep** (`/`).
- **Multi-pod tail** (`L`): follow many pods in one pane — the marked rows, or every pod the current filter shows. Each line is prefixed with its pod name in its own colour, and `/` greps pod names as well as line text.
- **Interactive exec shell** into a container (auto-fallback `bash` → `sh`).
- **Live CPU/MEM graphs** (sparklines) for pods and nodes.
- **Actions**: describe (YAML + events), edit YAML in `$EDITOR`, scale, rollout restart, **rollout undo**, delete, cordon/uncordon & drain nodes, **reveal secret** values, port-forward (pods *and* services).
- **Filter** (any-column substring) & **sort** by column (duration- and number-aware).
- **Safe secrets**: values are always masked (`<redacted: N bytes>`) when describing; plain-text reveal (`v`) is a separate, confirmed action.
- **Tabs** (`t` new, `w` close, `[`/`]` switch, `Alt`+`N` jump): keep several independent view sessions open — each remembers its own resource, namespace, filter, sort, and selection — for side-by-side monitoring.
- **Split view** (`|` columns, `-` rows, `+` 2×2 grid, `\` switch pane, `_` close pane): show **up to four tabs at once** — e.g. Pods beside Deployments, or the same resource in four namespaces. Pressing `|`/`-` again adds another pane (up to four, then unsplits). Every pane refreshes live; keys drive the focused one.
- **Workspaces** (`:ws save|load|rm|list [name]`): persist the whole layout — which tabs are open, what each points at (including CRDs), their names, namespaces, filters and sorts, plus the split arrangement. A workspace saved as `default` is restored at startup; quitting always writes `last`, so a layout closed by accident comes back with `:ws load last`.
- **Context switching** (`x`): switch cluster/context at runtime, no restart.
- **Help overlay** (`?`): every key binding + the `:jump` aliases.
- **Optional config file**: default namespace, refresh cadence, accent colour, and custom `:jump` aliases (`~/.config/kcli/config.yaml`).
- **Multi-select** (`Space`): mark rows and bulk-delete them in one confirmation (Delete-capable views).
- **Corner GIF animation**: play a `.gif` in the bottom-right corner of the main screen, rendered with 2×2 Unicode quadrant blocks (`$KCLI_SPLASH`); toggle with `a`.

---

## Requirements

- **Go 1.26+** (to build/install). In this environment the Go toolchain lives at `/usr/local/go/bin`, which may not be on `PATH` by default.
- Access to a Kubernetes cluster via kubeconfig, or running inside a cluster (in-cluster config).
- **metrics-server** (optional) for the CPU/MEM columns and graphs.
- A real terminal (PTY). kcli does not run over a plain pipe.

---

## Installation

### `go install` (recommended)

```bash
export PATH=$PATH:/usr/local/go/bin      # if the Go toolchain isn't already on PATH

# Straight from GitHub:
go install github.com/teknik-github/kcli@latest

# Or from a local checkout of this repository:
go install .
```

`go install` places the `kcli` binary in `$(go env GOPATH)/bin` (default `~/go/bin`). Make sure that directory is on your `PATH`, then run it:

```bash
export PATH=$PATH:$(go env GOPATH)/bin
kcli
```

### Build from source

```bash
export PATH=$PATH:/usr/local/go/bin

go build -o kcli .     # compile the binary into the current directory
./kcli                 # run (needs a real terminal + a reachable cluster)
```

Other development commands:

```bash
go vet ./...                 # static analysis
gofmt -w internal/ main.go   # format
```

---

## Configuration

kcli resolves the cluster connection in this order:

1. The **`$KUBECONFIG`** environment variable
2. **`~/.kube/config`**
3. **In-cluster config** (when running as a Pod)

The active context is shown in the header and starts as the kubeconfig's current-context. Switch to another context at runtime with `x`.

### Config file (optional)

kcli reads an optional YAML config from `$KCLI_CONFIG`, else `$XDG_CONFIG_HOME/kcli/config.yaml`, else `~/.config/kcli/config.yaml`. A missing or malformed file is ignored (defaults apply) — it never blocks startup.

Saved workspaces live in `workspaces.yaml` in that same directory. It is the one file kcli writes; the config file stays yours to edit.

### Corner GIF animation (optional)

Set `$KCLI_SPLASH` to a `.gif` path and kcli plays it, looping, in the bottom-right corner of the main screen. It is rendered with 2×2 Unicode **quadrant blocks** (four sub-pixels per cell, a best-fit glyph + fg/bg colour pair — like `chafa`) over antialiased (area-averaged) downscaling, which keeps it legible even in a small box. Truecolor terminal recommended. It starts on launch and does not steal focus. Press `a` to toggle it off/on. Unset (or a bad path) simply shows nothing.

Per-cell colours are chosen by a 1-step 2-means split (the two most distant pixels seed foreground/background), which keeps edges truer than a plain luminance threshold.

`$KCLI_SPLASH_SIZE` (`"WxH"` in cells, default `40x20`) sets the box size — larger means more detail but more screen.

`$KCLI_SPLASH_MODE=sextant` switches to 2×3 sextant glyphs (U+1FB00 "Legacy Computing"), ~50% more vertical detail than the default `quadrant`. Use it only if your terminal font renders those glyphs — otherwise they show as tofu boxes; quadrant is the universal default.

```bash
KCLI_SPLASH=~/pics/logo.gif KCLI_SPLASH_SIZE=60x30 KCLI_SPLASH_MODE=sextant kcli
```

```yaml
namespace: default        # startup namespace ("" / omitted = all namespaces)
refreshInterval: 5s        # auto-refresh cadence (>= 1s; default 3s)
theme: green               # accent colour name for tabs/header/selection
aliases:                   # custom :jump aliases -> resource name
  p: pods
  dp: deployments
```

Custom aliases are expanded before the built-in resolution, so `:p` → Pods, and any name the cluster knows (including CRDs) still works.

---

## Usage

On launch, kcli shows Pods across all namespaces. Switch resources with the number keys `1`–`9`, `Tab`/`Shift-Tab`, or `:` command-jump (for views past the ninth, and for CRDs). Change namespace with `n`. Press `?` any time for the full key list.

### Screen layout

```
┌ header: context / namespace / resource + counts            [ KCLI logo ]
├ tab bar: 1:Pods  2:Deployments  3:DaemonSets  …  (active view highlighted)
├ resource table (selected row highlighted)
└ footer: key bindings
```

### Key bindings

| Key                 | Action                                                          |
|---------------------|-----------------------------------------------------------------|
| `1`–`9`             | Jump directly to the Nth view (first nine)                      |
| `0`                 | Pulse — cluster health summary (`Enter` opens the kind)         |
| `:`                 | Command-jump by name/alias — any view, plus CRDs & any GVR      |
| `Tab` / `Shift-Tab` | Cycle to the next / previous view                               |
| `t` / `w`           | New tab (clones current view) / close tab                       |
| `T`                 | Rename the active tab (empty name reverts to the auto label)    |
| `[` / `]`           | Previous / next tab                                             |
| `Alt`+`1`–`9`       | Jump to the Nth tab                                             |
| `\|` / `-`           | Split into columns / rows; same key again adds a pane (up to 4), then unsplits |
| `+`                 | 2×2 quad view (same key again unsplits)                         |
| `_`                 | Close the focused pane (its tab stays open)                     |
| `\`                 | Move focus to the next split pane                               |
| `?`                 | Help overlay (all keys + `:jump` aliases)                       |
| `Enter`             | Resource detail (`describe` YAML + events)                      |
| `/`                 | Filter (any-column substring; empty submit or `Esc` clears)     |
| `.`                 | Cycle the sort column (wraps, including "no sort")             |
| `,`                 | Flip sort direction (ascending/descending)                     |
| `n`                 | Namespace picker (`<all>` for every namespace)                 |
| `x`                 | Context picker (switch cluster/context; `*` marks the current) |
| `r`                 | Manual refresh                                                  |
| `a`                 | Toggle the corner GIF animation (when `$KCLI_SPLASH` is set)    |
| `l`                 | Logs (follow; inside: `p` toggle previous, `/` grep, `q`/`Esc` close) |
| `L`                 | Tail many pods at once (marked rows, else every visible row)     |
| `e`                 | Interactive exec shell                                          |
| `E`                 | Edit YAML in `$EDITOR` and apply on save                        |
| `g`                 | Live CPU/MEM graph                                              |
| `s`                 | Scale (change replica count)                                    |
| `R`                 | Rollout restart                                                 |
| `u`                 | Rollout undo (roll back to the previous revision)              |
| `v`                 | Reveal secret values in plain text (confirmed)                 |
| `c`                 | Cordon / uncordon a node                                        |
| `D`                 | Drain a node (cordon + evict pods)                             |
| `f`                 | Start a port-forward (pod or service)                          |
| `F`                 | Open the Port-Forward view                                      |
| `Space`             | Mark / unmark the current row (multi-select)                   |
| `d`                 | Delete — the marked rows if any, else the current row          |
| `q`                 | Quit (in a hidden view — Port-Fwd/Dynamic — return instead)    |

> Actions are *data-driven*: a key only applies in views that support it (see the table below). Pressing `s` in Pods, for example, does nothing.

### Actions per view

| View          | Available actions                                     |
|---------------|-------------------------------------------------------|
| Pods          | logs, exec, graph, edit, delete, port-forward         |
| Deployments   | scale, restart, **undo**, edit, delete                |
| DaemonSets    | restart, **undo**, edit, delete                       |
| StatefulSets  | scale, restart, **undo**, edit, delete                |
| ReplicaSets   | edit, delete                                          |
| Nodes         | graph, cordon/uncordon, drain                         |
| Services      | edit, delete, **port-forward**                        |
| Secrets       | edit, delete, **reveal** (`v`)                        |
| Ingresses, Jobs, CronJobs, ConfigMaps, PVCs | edit, delete            |
| Events        | read-only (`Enter` for YAML)                          |
| Port-Fwd      | `Enter` view the forward's live log; `d` stop it; `q` go back |
| Dynamic/CRD (`:`) | read-only (`Enter` for YAML); `Tab`/`:`/`q` to leave |

### Feature notes

- **Command-jump** (`:`): type a resource name or short alias (`svc`, `deploy`, `cj`, `ev`, `pf`, …). Registered views switch instantly; anything else is resolved against the cluster's discovery info and opened as a generic view — this is how CRDs and any other GVR are reached. Short names resolve the same way kubectl does.
- **Pulse** (`0`): a summary row per kind — TOTAL / OK / WARN / HEALTH / DETAIL. Health comes from whatever signal the kind has: its status column (Pods, PVCs, Nodes, Events) or its `n/m` READY / COMPLETIONS column (Deployments, StatefulSets, DaemonSets, Jobs); kinds with neither (Services, Ingresses) just report a count. DETAIL names the top three problems with counts. A kind that fails to list shows `ERR` in its own row rather than blanking the screen. It respects the current namespace and reuses the other views' fetchers, so it needs no extra listers.
- **Dynamic / CRD view**: read-only. Generic NAMESPACE/NAME/AGE columns with full YAML detail on `Enter`. `Tab`, another `:`, or `q` leaves it (it is not in the numbered tab bar).
- **Workspaces** (`:ws`): `:ws save [name]` snapshots the layout, `:ws load [name]` restores it, `:ws rm [name]` deletes one, `:ws list` (or a bare `:ws`) shows what is saved; the name defaults to `default`, which is also the one restored automatically at startup — saving under it is how you opt in. Quitting overwrites the `last` slot. Only the *shape* of a session is stored (resource, namespace, filter, sort, tab name, split); rows always reload from the cluster. A tab parked on a CRD stores its GVR, so it comes back without another discovery lookup. Tabs whose resource no longer exists are skipped, and a stored split is only restored if it still describes 2–4 distinct, existing tabs. The file is `workspaces.yaml`, beside `config.yaml`.
- **Split view** (`|`, `-`, `+`, `\`, `_`): a layout over the tabs, not a second kind of session — each pane shows one tab, two to four of them. `|` and `-` open two panes and each further press adds one (a fourth press unsplits); `+` opens a 2×2 quad straight away and fills rows two at a time, so three panes leave the last one full width. Splitting with fewer tabs than panes clones the current session to fill them. The focused pane is the live one (accent border); the others are greyed and read-only until you `\` into them (focus cycles round), and all of them reload on the same refresh tick. Every other key (filter, sort, actions, view switch) applies to the focused pane only. `_` closes the focused pane but keeps its tab open; closing tabs (`w`) below the pane count sheds panes, and down to one tab it unsplits automatically.
- **Logs**: follows the last 500 lines by default. Press `p` to switch to the **previous** container instance's logs (useful for `CrashLoopBackOff`), and `/` to **grep** the buffer (filters in place, keeps following).
- **Multi-pod tail** (`L`): one pane, one follow stream per pod, capped at 20 pods (the extras are dropped and the title says so, e.g. `tail: 20 of 29 pods`). Targets are the marked rows when any are marked, otherwise every row the filter leaves on screen — so `/nginx` then `L` tails a whole deployment. Each pod gets a coloured name prefix (`ns/name` when the tail spans namespaces); the pane's `/` grep matches the prefix as well as the line, so it can narrow back to a single pod. Follows the first regular container of each pod, like `kubectl logs` with no `-c`.
- **Exec**: the TUI is *suspended* and the terminal is handed to the shell; it resumes automatically when the shell exits. Multi-container pods show a container picker (init containers first).
- **Edit** (`E`): fetches the live YAML (no events, secrets unmasked so values round-trip), opens it in `$EDITOR` (the TUI suspends), and on save applies it with an Update via the dynamic client. Unchanged buffers are a no-op.
- **Rollout restart** (`R`): stamps the `kubectl.kubernetes.io/restartedAt` annotation — identical to `kubectl rollout restart`.
- **Rollout undo** (`u`): rolls back to the previous revision, reconstructed client-side (no server endpoint exists) — Deployments restore the prior ReplicaSet's pod template; StatefulSets/DaemonSets re-apply the previous `ControllerRevision`. Reports "no previous revision" when there is nothing to undo.
- **Reveal secret** (`v`): after a confirmation, decodes and shows the secret's values in plain text — deliberately separate from `describe`, which always masks.
- **Port-forward** works on Pods and Services (a Service resolves to a Ready backing pod first, translating the service port to the pod's targetPort). Forwards bind `$KCLI_PF_ADDRESS` (comma-separated; default `0.0.0.0`, all interfaces), so the port is reachable from other hosts — handy when kcli runs on a remote server over SSH, but note `0.0.0.0` exposes the forwarded port to your network. Set `KCLI_PF_ADDRESS=127.0.0.1` to keep forwards loopback-only. It runs in the background and stays alive after the dialog closes; the header shows `⇄ N`. Manage forwards from the Port-Fwd view (`F`): `Enter` opens a live, timestamped log of the forwarder's output (the "Forwarding from …" notices and any connection errors), `d` stops the selected forward.
- **Events** are sorted newest-first and poll more slowly than other views; TYPE `Normal` is green, `Warning` is red.
- **Multi-select** (`Space`): marks the current row (highlighted background) in any Delete-capable view; `d` then deletes every marked row after one confirmation showing the count. Marks are keyed by namespace/name so they survive filter and sort, and clear on any view/namespace/context switch.
- **Context switch** (`x`): rebuilds the client for the chosen kubeconfig context and reloads; the namespace, filter, and sort reset since they are cluster-specific. In-flight actions and background port-forwards keep running against the cluster they were started on.

---

## Architecture

Two packages under `internal/`:

### `internal/k8s` — cluster access

Wraps `client-go`. `Client` holds a typed `clientset`, an optional `*metricsv.Clientset` (best-effort), the `*rest.Config` (kept for streaming subresources like exec & port-forward), and lazily-built, cached discovery `RESTMapper` + `dynamic.Interface`. Each resource has a display struct flattened for the table, plus a lister that sorts by `(namespace, name)`. `Describe`/`Delete`/`Scale`/`RolloutRestart`/`RolloutUndo` are `kind`-string dispatchers.

```
client.go        listers (cache-backed, live fallback), dispatchers, display/toX structs, describe/delete/scale/restart/cordon/drain, secret reveal, service→pod
informer.go      shared informer cache: cachedObjects, onChange/Stop — live updates + fewer List calls
dynamic.go       cached RESTMapper (+ShortcutExpander) & dynamic client; ResolveResource / ListDynamic / DescribeDynamic (CRDs)
rollout.go       RolloutUndo (client-side: ReplicaSet template swap / ControllerRevision patch)
metrics.go       enrich CPU/MEM (best-effort) + usage for graphs
exec.go          interactive exec shell (SPDY + raw mode + resize)
portforward.go   port-forward (SPDY)
```

### `internal/ui` — the tview interface

```
registry.go      ★ single source of truth: the list of viewDefs
workspace.go     save/restore the tab + split layout (:ws)
pulse.go         cluster health summary (reuses every view's Fetch)
app.go           App (widget tree + state), refresh loop (loadCurrentView), header/tabs, logo
views.go         generic filter & sort (cellLess), resolveView (:jump), humanAge
pods.go          drawTable + key handler (onTableKey)
tabs.go          browser-style tabs (tabState sessions, workspace strip, rename)
split.go         2–4 pane split layout over the tabs (pane focus, parked-pane refresh)
selection.go     multi-select marks + bulk delete
modals.go        detail, logs (stream + grep), scale, delete, restart, rollback, cordon, drain, reveal, filter, namespace, :jump prompt
multilog.go      multi-pod tail (fan-out streams, coloured pod prefixes, grep)
dynamic.go       jumpToView / jumpDynamic / setDynamicView (generic CRD view)
help.go          the `?` help overlay
edit.go          edit YAML in $EDITOR and apply
graph.go         sampler + CPU/MEM sparkline rendering
splash.go        optional corner GIF animation (quadrant-block renderer + loop player)
exec.go          suspend TUI → exec → resume
portforward.go   port-forward state + the built-in Port-Fwd view (pods & services)
```

### Core pattern: the view registry

`registry.go` is the single source of truth for what resources exist. Everything else is generic. A resource is one `*viewDef`:

```go
type viewDef struct {
    ID              string        // singular kind for Describe/Delete/Scale ("pod", …)
    Aliases         []string      // extra :jump keywords ("po", "svc", "cj", …)
    Title           string
    Columns         []string
    StatusCol       int           // column to color as a status, -1 = none
    ClusterScoped   bool          // no namespace (nodes)
    Local           bool          // rows come from App state, not the cluster (Port-Fwd)
    Hidden          bool          // off the tab bar / Tab cycling; reached via :jump (Dynamic slot)
    Dynamic         bool          // generic view backed by the dynamic client (CRDs / any GVR)
    RefreshInterval time.Duration // per-view auto-refresh cadence; 0 = default (3s)
    Caps            viewCaps      // which actions apply (Logs/Exec/Scale/Graph/Restart/Rollback/Reveal/…)
    Fetch           func(ctx, *k8s.Client, ns) ([]Row, error)
}
```

Every resource is flattened into the uniform `Row{Namespace, Name, Cells}`. Filtering, sorting, selection, detail, and actions all read through `filteredRows()`, so they stay consistent. Number keys `1`–`9` reach the first nine views; everything else (and any CRD) is reached with `:` — `resolveView` matches ID/alias/title, falling back to a discovery lookup that opens the generic **Dynamic** view.

### Adding a new resource

**Just add one `viewDef` + one lister in `k8s`. No per-resource `switch` statements to touch.** A new resource automatically gets a tab, a `:jump` alias, filter, sort, detail, and any actions declared in its `Caps`.

1. In `internal/k8s/client.go`: add a display struct + a lister (sorting by `(ns, name)`), then register the kind in the `Delete`/`Describe`/`Scale`/`RolloutRestart`/`RolloutUndo` dispatchers where relevant.
2. In `internal/ui/registry.go`: add one `viewDef` whose `Fetch` calls that lister (and any `Aliases`).

### Concurrency invariant (important)

`QueueUpdateDraw` **blocks** until the tview event loop drains it, and that loop only runs inside `tv.Run()`. So the first refresh must never be called synchronously before `Run()` (it would deadlock) — `autoRefresh` runs in its own goroutine. All background work mutates widgets/state only inside a `QueueUpdateDraw` closure — the only place it is safe to do so — so no locks are used.

Shared state is **read on the UI goroutine, never a background one**: `refresh()` is `QueueUpdate(loadCurrentView)`, which reads `viewIdx`/`client`/`namespace` on the UI goroutine and captures them as locals before spawning the fetch. Every action that starts a goroutine likewise captures `cl := a.client` first — this is race-free *and* pins the action to the cluster it started on, so a runtime context switch (`x`, which reassigns `a.client`) can't redirect it mid-flight. The only cross-goroutine field is `refreshEvery` (`atomic.Int64`, the ticker's cadence).

**Live updates** come from shared informers (`internal/k8s/informer.go`): resource listers read from an in-memory cache, and each informer's change handler calls a callback that nudges a bounded `watchTrigger` channel. `watchLoop` debounces those nudges (400 ms) into a `refresh()`, so edits/creates/deletes show up without waiting for the poll. Un-watchable resources fall back to a live List transparently; the periodic poll remains for metrics and as a safety net.

---

## Testing TUI changes

There is no permanent test suite. Two ways to verify:

- **Pure logic** — write a throwaway `*_test.go` (e.g. `cellLess`, `maskSecret`, `humanAge`, `toPod`), run `go test ./internal/...`, then delete it.
- **Rendering / interaction** — needs a real PTY. Drive it from Python: `pty.fork()`, `os.execvp("./kcli", …)`, size it with `TIOCSWINSZ` (wide, e.g. 200×45), feed keystrokes with `os.write`, read the screen back. Note: `tview` positions text with cursor-move escapes, so after stripping escapes words are concatenated; non-ASCII glyphs (sparklines, box-drawing) survive only in the raw bytes.

---

## Known limitations

- Filter is a substring match across visible columns; no label-selector support yet.
- Edit applies changes with a PUT (like `kubectl edit`), not server-side apply.
- The generic Dynamic/CRD view is read-only (list + YAML detail); actions there aren't wired.
- Rollout undo goes to the *immediately* previous revision only (no `--to-revision`).

---

## Stack

[`tview`] · [`tcell`] · [`client-go`] · [`metrics`] · [`asciigraph`]

[k9s]: https://k9scli.io
[`tview`]: https://github.com/rivo/tview
[`tcell`]: https://github.com/gdamore/tcell
[`client-go`]: https://github.com/kubernetes/client-go
[`metrics`]: https://github.com/kubernetes/metrics
[`asciigraph`]: https://github.com/guptarohit/asciigraph
