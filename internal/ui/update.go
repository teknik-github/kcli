package ui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/teknik-github/kcli/internal/version"
)

// checkForUpdate asks the module proxy once, at startup, whether a newer
// release exists and — only if so — records it so drawHeader can show an
// "Update: ↑ vX.Y.Z available (:update)" line. It is best-effort: any failure
// (offline, proxy down, unparseable version) is swallowed, and a "(devel)" or
// pseudo-version build never triggers the prompt (IsNewer rejects those).
// Runs on its own goroutine; the state write goes through QueueUpdateDraw.
func (a *App) checkForUpdate() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	latest, err := version.Latest(ctx)
	if err != nil || !version.IsNewer(latest, version.Current()) {
		return
	}
	a.tv.QueueUpdateDraw(func() {
		a.latestVersion = latest
		a.drawHeader()
	})
}

// updateCommand handles ":update". It self-updates by re-running the same
// `go install` that installed kcli — the only distribution channel — so it
// needs the Go toolchain on PATH; without it, it shows the manual command
// instead of failing silently.
func (a *App) updateCommand() {
	goBin, err := exec.LookPath("go")
	if err != nil {
		a.showMessage("update", "Go toolchain not found on PATH.\n\nkcli is distributed via `go install`. Install Go, or update manually:\n\n  go install "+version.Module+"@latest")
		return
	}

	target := "@latest"
	if a.latestVersion != "" {
		target = a.latestVersion
	}
	msg := fmt.Sprintf("Update kcli to %s?\n\nRuns:  go install %s@latest\nCurrent: %s\n\nRestart kcli afterwards to run the new build.",
		target, version.Module, version.Current())
	a.confirm("update", msg, "Update", func() { a.runUpdate(goBin) })
}

// runUpdate suspends the TUI and runs `go install module@latest` with the real
// terminal attached (so the user sees the download/build), then waits for a
// keypress before restoring the UI. The running process is still the old build
// until the user restarts — the message says so.
func (a *App) runUpdate(goBin string) {
	a.tv.Suspend(func() {
		fmt.Printf("\n\033[1mkcli:\033[0m running go install %s@latest ...\n\n", version.Module)
		cmd := exec.Command(goBin, "install", version.Module+"@latest")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Printf("\n\033[31mupdate failed: %v\033[0m\n", err)
		} else {
			fmt.Printf("\n\033[32mupdated. restart kcli to run the new version.\033[0m\n")
		}
		fmt.Print("\npress Enter to return to kcli...")
		bufio.NewReader(os.Stdin).ReadString('\n')
	})
}
