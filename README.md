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

- **14 resource views**: Pods, Deployments, DaemonSets, Services, Nodes, StatefulSets, ReplicaSets, PVCs, Ingresses, Jobs, CronJobs, ConfigMaps, Secrets, Events — plus a built-in **Port-Forward** view.
- **Command-jump** (`:`): jump to any view by name or short alias (`:svc`, `:cj`, `:ev`, …).
- **Generic / CRD view**: `:` any resource the cluster knows — CustomResourceDefinitions and built-ins without a dedicated view — resolved through discovery (kubectl-style short names) and listed read-only (name/age + YAML detail).
- **Auto-refresh** every 3 seconds (per-view cadence; Events poll slower).
- **Live metrics** (CPU/MEM columns + graphs) via metrics-server — *best-effort*; if metrics-server is absent it never errors, the columns just render `-`.
- **Log streaming (follow)** with a toggle to a crashed container's *previous* logs, and an in-pane **grep** (`/`).
- **Interactive exec shell** into a container (auto-fallback `bash` → `sh`).
- **Live CPU/MEM graphs** (sparklines) for pods and nodes.
- **Actions**: describe (YAML + events), edit YAML in `$EDITOR`, scale, rollout restart, **rollout undo**, delete, cordon/uncordon & drain nodes, **reveal secret** values, port-forward (pods *and* services).
- **Filter** (any-column substring) & **sort** by column (duration- and number-aware).
- **Safe secrets**: values are always masked (`<redacted: N bytes>`) when describing; plain-text reveal (`v`) is a separate, confirmed action.
- **Context switching** (`x`): switch cluster/context at runtime, no restart.
- **Help overlay** (`?`): every key binding + the `:jump` aliases.

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
| `:`                 | Command-jump by name/alias — any view, plus CRDs & any GVR      |
| `Tab` / `Shift-Tab` | Cycle to the next / previous view                               |
| `?`                 | Help overlay (all keys + `:jump` aliases)                       |
| `Enter`             | Resource detail (`describe` YAML + events)                      |
| `/`                 | Filter (any-column substring; empty submit or `Esc` clears)     |
| `.`                 | Cycle the sort column (wraps, including "no sort")             |
| `,`                 | Flip sort direction (ascending/descending)                     |
| `n`                 | Namespace picker (`<all>` for every namespace)                 |
| `x`                 | Context picker (switch cluster/context; `*` marks the current) |
| `r`                 | Manual refresh                                                  |
| `l`                 | Logs (follow; inside: `p` toggle previous, `/` grep, `q`/`Esc` close) |
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
| `d`                 | Delete the resource (with confirmation)                        |
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
| Port-Fwd      | `Enter`/`d` stop the selected forward; `q` go back    |
| Dynamic/CRD (`:`) | read-only (`Enter` for YAML); `Tab`/`:`/`q` to leave |

### Feature notes

- **Command-jump** (`:`): type a resource name or short alias (`svc`, `deploy`, `cj`, `ev`, `pf`, …). Registered views switch instantly; anything else is resolved against the cluster's discovery info and opened as a generic view — this is how CRDs and any other GVR are reached. Short names resolve the same way kubectl does.
- **Dynamic / CRD view**: read-only. Generic NAMESPACE/NAME/AGE columns with full YAML detail on `Enter`. `Tab`, another `:`, or `q` leaves it (it is not in the numbered tab bar).
- **Logs**: follows the last 500 lines by default. Press `p` to switch to the **previous** container instance's logs (useful for `CrashLoopBackOff`), and `/` to **grep** the buffer (filters in place, keeps following).
- **Exec**: the TUI is *suspended* and the terminal is handed to the shell; it resumes automatically when the shell exits. Multi-container pods show a container picker (init containers first).
- **Edit** (`E`): fetches the live YAML (no events, secrets unmasked so values round-trip), opens it in `$EDITOR` (the TUI suspends), and on save applies it with an Update via the dynamic client. Unchanged buffers are a no-op.
- **Rollout restart** (`R`): stamps the `kubectl.kubernetes.io/restartedAt` annotation — identical to `kubectl rollout restart`.
- **Rollout undo** (`u`): rolls back to the previous revision, reconstructed client-side (no server endpoint exists) — Deployments restore the prior ReplicaSet's pod template; StatefulSets/DaemonSets re-apply the previous `ControllerRevision`. Reports "no previous revision" when there is nothing to undo.
- **Reveal secret** (`v`): after a confirmation, decodes and shows the secret's values in plain text — deliberately separate from `describe`, which always masks.
- **Port-forward** works on Pods and Services (a Service resolves to a Ready backing pod first). It runs in the background and stays alive after the dialog closes; the header shows `⇄ N`. Manage/stop forwards from the Port-Fwd view (`F`).
- **Events** are sorted newest-first and poll more slowly than other views; TYPE `Normal` is green, `Warning` is red.
- **Context switch** (`x`): rebuilds the client for the chosen kubeconfig context and reloads; the namespace, filter, and sort reset since they are cluster-specific. In-flight actions and background port-forwards keep running against the cluster they were started on.

---

## Architecture

Two packages under `internal/`:

### `internal/k8s` — cluster access

Wraps `client-go`. `Client` holds a typed `clientset`, an optional `*metricsv.Clientset` (best-effort), the `*rest.Config` (kept for streaming subresources like exec & port-forward), and lazily-built, cached discovery `RESTMapper` + `dynamic.Interface`. Each resource has a display struct flattened for the table, plus a lister that sorts by `(namespace, name)`. `Describe`/`Delete`/`Scale`/`RolloutRestart`/`RolloutUndo` are `kind`-string dispatchers.

```
client.go        listers, dispatchers, display structs, describe/delete/scale/restart/cordon/drain, secret reveal, service→pod
dynamic.go       cached RESTMapper (+ShortcutExpander) & dynamic client; ResolveResource / ListDynamic / DescribeDynamic (CRDs)
rollout.go       RolloutUndo (client-side: ReplicaSet template swap / ControllerRevision patch)
metrics.go       enrich CPU/MEM (best-effort) + usage for graphs
exec.go          interactive exec shell (SPDY + raw mode + resize)
portforward.go   port-forward (SPDY)
```

### `internal/ui` — the tview interface

```
registry.go      ★ single source of truth: the list of viewDefs
app.go           App (widget tree + state), refresh loop (loadCurrentView), header/tabs, logo
views.go         generic filter & sort (cellLess), resolveView (:jump), humanAge
pods.go          drawTable + key handler (onTableKey)
modals.go        detail, logs (stream + grep), scale, delete, restart, rollback, cordon, drain, reveal, filter, namespace, :jump prompt
dynamic.go       jumpToView / jumpDynamic / setDynamicView (generic CRD view)
help.go          the `?` help overlay
edit.go          edit YAML in $EDITOR and apply
graph.go         sampler + CPU/MEM sparkline rendering
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
