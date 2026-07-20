# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`kcli` — an interactive terminal UI (TUI) for managing Kubernetes, built with `tview`/`tcell` and the official `client-go`. A lightweight k9s-style browser: multi-resource views, live metrics, logs, exec, scale, delete, port-forward.

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

Two packages under `internal/`:

- **`internal/k8s`** — all cluster access (`client-go`). `Client` wraps a typed `clientset`, an optional `*metricsv.Clientset` (best-effort), and the `*rest.Config` (kept for streaming subresources). Each resource has a display struct (`Pod`, `Deployment`, …) flattened for the table, plus a lister that sorts by `(namespace, name)`. `Describe`/`Delete`/`Scale` are `kind`-string dispatchers. `exec.go` and `portforward.go` hold the SPDY streaming subresources.
- **`internal/ui`** — the `tview` app. `App` (in `app.go`) owns the widget tree and mutable state.

### The view registry — the central pattern

`registry.go` is the single source of truth for what resources exist. Everything else is generic. A resource is one `*viewDef` appended to `resourceViews`:

```go
type Row struct{ Namespace, Name string; Cells []string } // uniform, display-ready
type viewDef struct {
    ID            string   // singular kind for Describe/Delete/Scale ("pod", ...)
    Title         string
    Columns       []string
    StatusCol     int      // column to color as a status, -1 = none
    ClusterScoped bool     // no namespace (nodes)
    Caps          viewCaps // which actions apply (Logs/Exec/Scale/Graph/Delete/PortForward)
    Fetch         func(ctx, *k8s.Client, ns) ([]Row, error) // list + map (+metrics enrich)
}
```

**To add a resource: add one `viewDef` + one client lister. Nothing else.** It automatically gets a tab, number-key, filter, sort, detail, and any actions declared in `Caps`. Do NOT reintroduce per-resource `switch` statements in `app.go`/`pods.go`/`views.go` — that was the pre-refactor design and was deliberately removed.

`filteredRows()` (`views.go`) filters then sorts `a.rows` generically; `drawTable`, `selectedRow`, `rowCount`, and `selectedName` all read through it, so filter/sort/selection stay consistent. Key handling (`onTableKey` in `pods.go`) is data-driven off `Caps`, not per-view `if` checks; number keys `1`-`9` map to `switchView(n-1)`.

### tview concurrency invariant (critical)

`QueueUpdateDraw` **blocks** until the event loop drains it, and that loop only runs inside `tv.Run()`. So the first refresh must never be called synchronously before `Run()` — it deadlocks and the screen shows nothing (only the statically-set footer). `autoRefresh` runs in its own goroutine for this reason. All background work (refresh, metrics, graph sampling, port-forward status, log/describe fetches) mutates UI state only inside a `QueueUpdateDraw` closure — that closure is the only place it's safe to touch widgets/state, so no locks are used.

### Long-running actions

- **Modals** live on a `tview.Pages` overlay: `openModal` (centered box), `openModalFull` (full screen). `closeModal` removes the page and refocuses the table.
- **exec** (`ui/exec.go`) uses `tv.Suspend` to hand the real terminal to an interactive shell, then resumes. `pickContainer` prompts on multi-container pods (init containers listed first); single-container skips the picker.
- **graph** (`ui/graph.go`) samples metrics on its own ticker into a ring buffer and renders an `asciigraph` line chart (CPU red / MEM blue via `asciigraph.SeriesColors` → `tview.TranslateANSI`; raw ANSI would otherwise be swallowed by the TextView). Sampler goroutine is stopped via `graphStop` on close.
- **port-forward** (`ui/portforward.go`) runs in the background, tracked in `App.forwards` and surfaced as `⇄ pf:N` in the header. The **Port-Fwd view** is a first-class tab (`viewDef` with `Local: true` — rows come from `App.forwards`, not the cluster; `refresh` special-cases `Local` and never calls `Fetch`). `F` jumps to it; Enter/`d` stops the selected forward (keyed by the ID column, which survives filter/sort). This is the model for any app-local (non-cluster) view.

### Secrets

`Secrets` lister carries only metadata. `Describe` masks values via `maskSecret` before rendering: markers go into `StringData` (not `Data`) because YAML base64-encodes `[]byte` `Data` — the `last-applied-configuration` annotation is stripped too.

### Cell sorting

`cellLess` (`views.go`) compares same-column cells: duration-aware first (`"5m" < "3d"`, fixing AGE), then leading-number numeric (`"12m" < "100m"`, CPU/MEM/restarts), then lexical. Safe because sorting only ever compares cells from one column, where units are homogeneous.
