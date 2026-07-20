package k8s

import (
	"fmt"
	"io"
	"net/http"

	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// PortForward forwards local ports to a pod. ports are "local:remote" strings
// (e.g. "8080:80"). It blocks until stopCh is closed, so callers should run it
// in a goroutine; readyCh is closed once forwarding is established.
func (c *Client) PortForward(namespace, pod string, ports []string, out, errOut io.Writer, stopCh <-chan struct{}, readyCh chan struct{}) error {
	req := c.clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(namespace).
		Name(pod).
		SubResource("portforward")

	transport, upgrader, err := spdy.RoundTripperFor(c.restCfg)
	if err != nil {
		return fmt.Errorf("build spdy transport: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", req.URL())

	fw, err := portforward.New(dialer, ports, stopCh, readyCh, out, errOut)
	if err != nil {
		return fmt.Errorf("init port-forward: %w", err)
	}
	return fw.ForwardPorts() // blocks until stopCh is closed
}
