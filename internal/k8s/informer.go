package k8s

import (
	"context"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// GVRs for the built-in resources kcli lists. Used to drive the shared informer
// cache; listers read from cache (falling back to a live List when a resource's
// informer cannot sync).
var (
	gvrPods         = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	gvrServices     = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	gvrNodes        = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	gvrConfigMaps   = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	gvrSecrets      = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	gvrPVCs         = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	gvrEvents       = schema.GroupVersionResource{Version: "v1", Resource: "events"}
	gvrDeployments  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	gvrDaemonSets   = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}
	gvrStatefulSets = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	gvrReplicaSets  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	gvrIngresses    = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	gvrJobs         = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	gvrCronJobs     = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}
)

// SetOnChange registers a callback invoked (from an informer goroutine) whenever
// any watched resource changes. The UI uses it to refresh live instead of only
// on the poll tick. Safe to call before or after informers start.
func (c *Client) SetOnChange(f func()) {
	c.infMu.Lock()
	c.onChange = f
	c.infMu.Unlock()
}

// Stop tears down all running informers for this client. Called when a context
// switch replaces the client, so the old cluster's watches don't linger.
func (c *Client) Stop() {
	c.infMu.Lock()
	if c.infStop != nil {
		close(c.infStop)
		c.infStop = nil
		c.infFactory = nil
		c.infStarted = nil
	}
	c.infMu.Unlock()
}

// ensureFactoryLocked lazily builds the shared informer factory. Caller holds infMu.
func (c *Client) ensureFactoryLocked() {
	if c.infFactory == nil {
		c.infFactory = informers.NewSharedInformerFactory(c.clientset, 0)
		c.infStop = make(chan struct{})
		c.infStarted = map[schema.GroupVersionResource]bool{}
	}
}

// fireChange invokes the registered onChange callback, if any.
func (c *Client) fireChange() {
	c.infMu.Lock()
	f := c.onChange
	c.infMu.Unlock()
	if f != nil {
		f()
	}
}

// cachedObjects returns a resource's objects from the shared informer cache,
// starting and syncing its informer on first use. The bool is false (with no
// error) when the informer could not sync within ctx — the caller then falls
// back to a live List, so an un-watchable resource still works. An empty ns
// lists across all namespaces.
func (c *Client) cachedObjects(ctx context.Context, gvr schema.GroupVersionResource, ns string) ([]runtime.Object, bool, error) {
	c.infMu.Lock()
	c.ensureFactoryLocked()
	factory := c.infFactory
	stop := c.infStop
	gi, err := factory.ForResource(gvr)
	if err != nil {
		c.infMu.Unlock()
		return nil, false, err
	}
	inf := gi.Informer()
	if !c.infStarted[gvr] {
		_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(interface{}) { c.fireChange() },
			UpdateFunc: func(interface{}, interface{}) { c.fireChange() },
			DeleteFunc: func(interface{}) { c.fireChange() },
		})
		c.infStarted[gvr] = true
		factory.Start(stop)
	}
	c.infMu.Unlock()

	if !cache.WaitForCacheSync(mergeStop(ctx, stop), inf.HasSynced) {
		return nil, false, nil // couldn't sync (ctx timeout / stopped): fall back to live
	}

	lister := gi.Lister()
	var objs []runtime.Object
	if ns != "" {
		objs, err = lister.ByNamespace(ns).List(labels.Everything())
	} else {
		objs, err = lister.List(labels.Everything())
	}
	return objs, true, err
}

// mergeStop returns a channel closed when either ctx is done or stop is closed —
// the stop signal WaitForCacheSync expects.
func mergeStop(ctx context.Context, stop <-chan struct{}) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-stop:
		}
		close(out)
	}()
	return out
}
