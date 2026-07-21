package k8s

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// ForwardBindAddresses returns the local addresses a forward binds, from
// $KCLI_PF_ADDRESS (comma-separated), defaulting to 0.0.0.0 (all IPv4
// interfaces — reachable from other hosts, e.g. when kcli runs on a remote
// server over SSH). Set it to 127.0.0.1 to keep forwards loopback-only.
func ForwardBindAddresses() []string {
	v := strings.TrimSpace(os.Getenv("KCLI_PF_ADDRESS"))
	if v == "" {
		return []string{"0.0.0.0"}
	}
	var addrs []string
	for _, a := range strings.Split(v, ",") {
		if a = strings.TrimSpace(a); a != "" {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		return []string{"0.0.0.0"}
	}
	return addrs
}

// PortForward forwards local ports to a pod. ports are "local:remote" strings
// (e.g. "8080:80"). It binds ForwardBindAddresses(). It blocks until stopCh is
// closed, so callers should run it in a goroutine; readyCh is closed once
// forwarding is established.
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

	fw, err := portforward.NewOnAddresses(dialer, ForwardBindAddresses(), ports, stopCh, readyCh, out, errOut)
	if err != nil {
		return fmt.Errorf("init port-forward: %w", err)
	}
	return fw.ForwardPorts() // blocks until stopCh is closed
}
