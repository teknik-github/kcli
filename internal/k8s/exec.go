package k8s

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"golang.org/x/term"
)

// shellCommand tries bash, falling back to sh, so it works across images.
var shellCommand = []string{"/bin/sh", "-c", "exec /bin/bash 2>/dev/null || exec /bin/sh"}

// ExecShell attaches an interactive shell to the named container of a pod,
// wiring the process's own stdio. It puts the terminal into raw mode and
// forwards window-resize events, restoring the terminal on exit. An empty
// container lets the server pick (valid only for single-container pods).
//
// The caller must have released the terminal first (e.g. via the TUI's
// Suspend), since this takes over stdin/stdout/stderr.
func (c *Client) ExecShell(ctx context.Context, namespace, pod, container string) error {
	req := c.clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   shellCommand,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.restCfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("init executor: %w", err)
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin is not a terminal")
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("set raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)

	sizeQueue := newSizeQueue(fd)
	defer sizeQueue.stop()

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             os.Stdin,
		Stdout:            os.Stdout,
		Stderr:            os.Stderr,
		Tty:               true,
		TerminalSizeQueue: sizeQueue,
	})
}

// sizeQueue feeds terminal dimensions to the exec stream: one initial size,
// then a new size on every SIGWINCH.
type sizeQueue struct {
	fd   int
	ch   chan remotecommand.TerminalSize
	sig  chan os.Signal
	done chan struct{}
}

func newSizeQueue(fd int) *sizeQueue {
	q := &sizeQueue{
		fd:   fd,
		ch:   make(chan remotecommand.TerminalSize, 1),
		sig:  make(chan os.Signal, 1),
		done: make(chan struct{}),
	}
	q.push() // send the current size immediately
	signal.Notify(q.sig, syscall.SIGWINCH)
	go q.watch()
	return q
}

func (q *sizeQueue) push() {
	w, h, err := term.GetSize(q.fd)
	if err != nil {
		return
	}
	select {
	case q.ch <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}:
	default: // drop if the consumer hasn't drained the last size yet
	}
}

func (q *sizeQueue) watch() {
	for {
		select {
		case <-q.sig:
			q.push()
		case <-q.done:
			return
		}
	}
}

// Next implements remotecommand.TerminalSizeQueue.
func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case size := <-q.ch:
		return &size
	case <-q.done:
		return nil
	}
}

func (q *sizeQueue) stop() {
	signal.Stop(q.sig)
	close(q.done)
}
