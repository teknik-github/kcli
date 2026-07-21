package ui

import (
	"bufio"
	"context"
	"fmt"
	"os"
)

// execShell drops the user into an interactive shell in the selected pod,
// prompting for a container first on multi-container pods.
func (a *App) execShell() {
	row, ok := a.selectedRow()
	if !ok {
		return
	}
	a.pickContainer(row.Namespace, row.Name, func(container string) {
		a.execInto(row.Namespace, row.Name, container)
	})
}

// execInto suspends the TUI, attaches a shell to the given container, then
// resumes the TUI when the shell exits.
func (a *App) execInto(namespace, name, container string) {
	cl := a.client // pin the cluster this exec targets, in case the context switches
	// Suspend hands the terminal back to the OS while f runs, then restores the
	// tview screen afterwards — exactly what an interactive exec needs.
	a.tv.Suspend(func() {
		fmt.Printf("\n\033[1mkcli:\033[0m connecting to %s/%s [%s] (exit shell to return)\n\n",
			namespace, name, container)

		if err := cl.ExecShell(context.Background(), namespace, name, container); err != nil {
			fmt.Printf("\n\033[31mexec failed: %v\033[0m\n", err)
			fmt.Print("press Enter to return to kcli...")
			bufio.NewReader(os.Stdin).ReadString('\n')
		}
	})

	// Cluster state may have changed while the shell was open.
	go a.refresh()
}
