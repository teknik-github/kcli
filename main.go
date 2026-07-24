// Command kcli is an interactive terminal UI for managing Kubernetes.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"

	"github.com/teknik-github/kcli/internal/config"
	"github.com/teknik-github/kcli/internal/k8s"
	"github.com/teknik-github/kcli/internal/ui"
)

func main() {
	// client-go reports async failures — a port-forward stream resetting under
	// load, a watch dropping — through klog, which writes to stderr. Inside a
	// full-screen TUI that stderr text scribbles straight over the layout. The
	// errors that matter are surfaced in-app (a forward's status, a benchmark's
	// error column), so silence the raw logging two ways: point klog at a discard
	// logger (its own LogToStderr/SetOutput don't reliably mute error severity),
	// and drop the util/runtime handler that turns portforward stream failures
	// into "Unhandled Error" lines.
	klog.SetLogger(logr.Discard())
	utilruntime.ErrorHandlers = []utilruntime.ErrorHandler{
		func(context.Context, error, string, ...interface{}) {},
	}

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
