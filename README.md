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

A single binary with no runtime dependencies. It shows many resource kinds in tabs, with live metrics (CPU/MEM), streaming logs, exec shell, scale, rollout restart, port-forward, and more.

---

## Features

- **14 resource views**: Pods, Deployments, DaemonSets, Services, Nodes, StatefulSets, ReplicaSets, PVCs, Ingresses, Jobs, CronJobs, ConfigMaps, Secrets, Events — plus a built-in **Port-Forward** view.
- **Auto-refresh** every 3 seconds.
- **Live metrics** (CPU/MEM columns + graphs) via metrics-server — *best-effort*; if metrics-server is absent it never errors, the columns just render `-`.
- **Log streaming (follow)** with a toggle to a crashed container's *previous* logs.
- **Interactive exec shell** into a container (auto-fallback `bash` → `sh`).
- **Live CPU/MEM graphs** (sparklines) for pods and nodes.
- **Actions**: describe (YAML + events), scale, rollout restart, delete, cordon/uncordon nodes, port-forward.
- **Filter** (name/namespace substring) & **sort** by column (duration- and number-aware).
- **Safe secrets**: values are always masked (`<redacted: N bytes>`) when describing.

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

The active context is shown in the header. (There is no runtime context switching yet — kcli uses the current-context at startup.)

---

## Usage

On launch, kcli shows Pods across all namespaces. Switch resources with the number keys `1`–`9` or `Tab`/`Shift-Tab`. Change namespace with `n`.

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
| `1`–`9`             | Jump directly to the Nth view                                   |
| `Tab` / `Shift-Tab` | Cycle to the next / previous view                               |
| `Enter`             | Resource detail (`describe` YAML + events)                      |
| `/`                 | Filter (name/namespace substring; empty = clear)               |
| `.`                 | Cycle the sort column (wraps, including "no sort")             |
| `,`                 | Flip sort direction (ascending/descending)                     |
| `n`                 | Namespace picker (`<all>` for every namespace)                 |
| `r`                 | Manual refresh                                                  |
| `l`                 | Logs (follow mode; inside: `p` toggle previous, `q`/`Esc` close) |
| `e`                 | Interactive exec shell                                          |
| `g`                 | Live CPU/MEM graph                                              |
| `s`                 | Scale (change replica count)                                    |
| `R`                 | Rollout restart                                                 |
| `c`                 | Cordon / uncordon a node                                        |
| `f`                 | Start a port-forward                                            |
| `F`                 | Open the Port-Forward view                                      |
| `d`                 | Delete the resource (with confirmation)                        |
| `q`                 | Quit (in the Port-Forward view: return to the previous view)   |

> Actions are *data-driven*: a key only applies in views that support it (see the table below). Pressing `s` in Pods, for example, does nothing.

### Actions per view

| View          | Available actions                                     |
|---------------|-------------------------------------------------------|
| Pods          | logs, exec, graph, delete, port-forward               |
| Deployments   | scale, restart, delete                                |
| DaemonSets    | restart, delete                                       |
| StatefulSets  | scale, restart, delete                                |
| ReplicaSets   | delete                                                |
| Nodes         | graph, cordon/uncordon                                |
| Services, Ingresses, Jobs, CronJobs, ConfigMaps, Secrets, PVCs | delete |
| Events        | read-only (`Enter` for YAML)                          |
| Port-Fwd      | `Enter`/`d` stop the selected forward; `q` go back    |

### Feature notes

- **Logs**: follows the last 500 lines by default. Press `p` to switch to the **previous** container instance's logs (useful for `CrashLoopBackOff`).
- **Exec**: the TUI is *suspended* and the terminal is handed to the shell; it resumes automatically when the shell exits. Multi-container pods show a container picker (init containers first).
- **Rollout restart**: stamps the `kubectl.kubernetes.io/restartedAt` annotation — identical to `kubectl rollout restart`.
- **Port-forward** runs in the background and stays alive after the dialog closes; the header shows `⇄ pf:N`. Manage/stop forwards from the Port-Fwd view (`F`).
- **Events** are sorted newest-first; TYPE `Normal` is green, `Warning` is red.

---

## Architecture

Two packages under `internal/`:

### `internal/k8s` — cluster access

Wraps `client-go`. `Client` holds a typed `clientset`, an optional `*metricsv.Clientset` (best-effort), and the `*rest.Config` (kept for streaming subresources like exec & port-forward). Each resource has a display struct flattened for the table, plus a lister that sorts by `(namespace, name)`. `Describe`/`Delete`/`Scale`/`RolloutRestart` are `kind`-string dispatchers.

```
client.go        listers, dispatchers, display structs, describe/delete/scale/restart/cordon
metrics.go       enrich CPU/MEM (best-effort) + usage for graphs
exec.go          interactive exec shell (SPDY + raw mode + resize)
portforward.go   port-forward (SPDY)
```

### `internal/ui` — the tview interface

```
registry.go      ★ single source of truth: the list of viewDefs
app.go           App (widget tree + state), refresh loop, header/tabs, logo
views.go         generic filter & sort (cellLess), humanAge
pods.go          drawTable + key handler (onTableKey)
modals.go        detail, logs (streaming), scale, delete, restart, cordon, filter, namespace
graph.go         sampler + CPU/MEM sparkline rendering
exec.go          suspend TUI → exec → resume
portforward.go   port-forward state + the built-in Port-Fwd view
```

### Core pattern: the view registry

`registry.go` is the single source of truth for what resources exist. Everything else is generic. A resource is one `*viewDef`:

```go
type viewDef struct {
    ID            string       // singular kind for Describe/Delete/Scale ("pod", …)
    Title         string
    Columns       []string
    StatusCol     int          // column to color as a status, -1 = none
    ClusterScoped bool         // no namespace (nodes)
    Local         bool         // rows come from App state, not the cluster (Port-Fwd)
    Caps          viewCaps     // which actions apply (Logs/Exec/Scale/Graph/Restart/Delete/…)
    Fetch         func(ctx, *k8s.Client, ns) ([]Row, error)
}
```

Every resource is flattened into the uniform `Row{Namespace, Name, Cells}`. Filtering, sorting, selection, detail, and actions all read through `filteredRows()`, so they stay consistent.

### Adding a new resource

**Just add one `viewDef` + one lister in `k8s`. No per-resource `switch` statements to touch.** A new resource automatically gets a tab, a number key, filter, sort, detail, and any actions declared in its `Caps`.

1. In `internal/k8s/client.go`: add a display struct + a lister (sorting by `(ns, name)`), then register the kind in the `Delete`/`Describe`/`Scale`/`RolloutRestart` dispatchers where relevant.
2. In `internal/ui/registry.go`: add one `viewDef` whose `Fetch` calls that lister.

### Concurrency invariant (important)

`QueueUpdateDraw` **blocks** until the tview event loop drains it, and that loop only runs inside `tv.Run()`. So the first refresh must never be called synchronously before `Run()` (it would deadlock) — `autoRefresh` runs in its own goroutine. All background work (refresh, metrics, graph sampling, log streaming, port-forward status) mutates widgets/state only inside a `QueueUpdateDraw` closure — the only place it is safe to do so — so no locks are used.

---

## Testing TUI changes

There is no permanent test suite. Two ways to verify:

- **Pure logic** — write a throwaway `*_test.go` (e.g. `cellLess`, `maskSecret`, `humanAge`, `toPod`), run `go test ./internal/...`, then delete it.
- **Rendering / interaction** — needs a real PTY. Drive it from Python: `pty.fork()`, `os.execvp("./kcli", …)`, size it with `TIOCSWINSZ` (wide, e.g. 200×45), feed keystrokes with `os.write`, read the screen back. Note: `tview` positions text with cursor-move escapes, so after stripping escapes words are concatenated; non-ASCII glyphs (sparklines, box-drawing) survive only in the raw bytes.

---

## Known limitations

- No runtime context switching (uses the current-context at startup).
- Filter only matches name/namespace; no label-selector support yet.
- Describe is read-only (no YAML edit/apply).
- Nodes support cordon/uncordon only (no drain).

---

## Stack

[`tview`] · [`tcell`] · [`client-go`] · [`metrics`] · [`asciigraph`]

[k9s]: https://k9scli.io
[`tview`]: https://github.com/rivo/tview
[`tcell`]: https://github.com/gdamore/tcell
[`client-go`]: https://github.com/kubernetes/client-go
[`metrics`]: https://github.com/kubernetes/metrics
[`asciigraph`]: https://github.com/guptarohit/asciigraph
