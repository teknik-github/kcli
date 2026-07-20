// Command kcli is an interactive terminal UI for managing Kubernetes.
package main

import (
	"fmt"
	"os"

	"github.com/teknik-github/kcli/internal/k8s"
	"github.com/teknik-github/kcli/internal/ui"
)

func main() {
	client, err := k8s.NewClient(os.Getenv("KUBECONFIG"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "kcli: failed to connect to cluster: %v\n", err)
		os.Exit(1)
	}

	app := ui.NewApp(client)
	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "kcli: %v\n", err)
		os.Exit(1)
	}
}
