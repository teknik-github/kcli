// Command kcli is an interactive terminal UI for managing Kubernetes.
package main

import (
	"fmt"
	"os"

	"github.com/teknik-github/kcli/internal/config"
	"github.com/teknik-github/kcli/internal/k8s"
	"github.com/teknik-github/kcli/internal/ui"
)

func main() {
	// No explicit path: client-go resolves $KUBECONFIG (merging every file it
	// lists), then ~/.kube/config, then in-cluster config.
	client, err := k8s.NewClient("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "kcli: failed to connect to cluster: %v\n", err)
		os.Exit(1)
	}

	cfg, _ := config.Load()
	app := ui.NewApp(client, cfg)
	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "kcli: %v\n", err)
		os.Exit(1)
	}
}
