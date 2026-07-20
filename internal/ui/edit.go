package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// showEdit fetches the selected resource's YAML, opens it in $EDITOR (suspending
// the TUI, like exec), and applies the result on save if it changed — the
// `kubectl edit` flow. Only views whose Caps.Edit is set reach here.
func (a *App) showEdit() {
	kind, ns, name, ok := a.selectedName()
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		original, err := a.client.GetYAML(ctx, kind, ns, name)
		a.tv.QueueUpdateDraw(func() {
			if err != nil {
				a.showMessage("edit", fmt.Sprintf("error: %v", err))
				return
			}
			a.editYAML(ns, name, original)
		})
	}()
}

// editYAML opens original in the user's editor, then applies the edited YAML if
// it changed. Runs on the UI goroutine; Suspend hands the terminal to the
// editor and restores the TUI when it exits.
func (a *App) editYAML(ns, name, original string) {
	var edited string
	var editErr error
	a.tv.Suspend(func() {
		edited, editErr = runEditor(original)
	})

	switch {
	case editErr != nil:
		a.showMessage("edit", fmt.Sprintf("editor error: %v", editErr))
		return
	case strings.TrimSpace(edited) == strings.TrimSpace(original):
		a.showMessage("edit", "no changes")
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := a.client.ApplyYAML(ctx, []byte(edited))
		a.tv.QueueUpdateDraw(func() {
			if err != nil {
				a.showMessage("edit", fmt.Sprintf("apply failed: %v", err))
			} else {
				a.showMessage("edit", fmt.Sprintf("applied %s/%s", ns, name))
			}
		})
		a.refresh()
	}()
}

// runEditor writes content to a temp file, opens it in $EDITOR (falling back to
// vi), and returns the edited content. The caller must have released the
// terminal first (via the TUI's Suspend).
func runEditor(content string) (string, error) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}

	f, err := os.CreateTemp("", "kcli-*.yaml")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return "", err
	}
	f.Close()

	// via `sh -c` so $EDITOR may carry arguments (e.g. "code -w", "vim -u NONE").
	cmd := exec.Command("sh", "-c", editor+" \""+path+"\"")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
