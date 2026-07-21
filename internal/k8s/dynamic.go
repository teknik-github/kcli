package k8s

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

// restMapper lazily builds and caches a discovery-backed REST mapper. It maps a
// resource/kind/short-name (e.g. "po", "deployments", a CRD plural) to its GVR
// and scope, which is what powers generic listing and `:jump` to any resource.
func (c *Client) restMapper() (meta.RESTMapper, error) {
	if c.mapper != nil {
		return c.mapper, nil
	}
	disco, err := discovery.NewDiscoveryClientForConfig(c.restCfg)
	if err != nil {
		return nil, err
	}
	groups, err := restmapper.GetAPIGroupResources(disco)
	if err != nil {
		return nil, err
	}
	// Wrap with the shortcut expander so short names (po, pv, cm, deploy, …) and
	// CRD-declared short names resolve, exactly as kubectl does.
	base := restmapper.NewDiscoveryRESTMapper(groups)
	c.mapper = restmapper.NewShortcutExpander(base, disco, func(string) {})
	return c.mapper, nil
}

// dynamicClient lazily builds and caches the dynamic client.
func (c *Client) dynamicClient() (dynamic.Interface, error) {
	if c.dynClient != nil {
		return c.dynClient, nil
	}
	dc, err := dynamic.NewForConfig(c.restCfg)
	if err != nil {
		return nil, err
	}
	c.dynClient = dc
	return dc, nil
}

// DynRow is the minimal, kind-agnostic shape a generic (dynamic/CRD) view shows:
// just namespace, name, and age — the columns available for any resource.
type DynRow struct {
	Namespace string
	Name      string
	Age       string
}

// ResolveResource maps a `:jump` query (plural, singular, short name, or
// "kind.group") to a concrete GVR plus whether it is namespaced and its Kind.
// This is what lets the command-jump reach CRDs and any built-in the registry
// does not carry an explicit view for.
func (c *Client) ResolveResource(query string) (gvr schema.GroupVersionResource, namespaced bool, kind string, err error) {
	m, err := c.restMapper()
	if err != nil {
		return schema.GroupVersionResource{}, false, "", err
	}
	gvr, err = m.ResourceFor(schema.GroupVersionResource{Resource: query})
	if err != nil {
		return schema.GroupVersionResource{}, false, "", err
	}
	gvk, err := m.KindFor(gvr)
	if err != nil {
		return schema.GroupVersionResource{}, false, "", err
	}
	mapping, err := m.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, false, "", err
	}
	return gvr, mapping.Scope.Name() == meta.RESTScopeNameNamespace, gvk.Kind, nil
}

// resourceInterface returns the dynamic client scoped correctly: a namespaced
// resource with an empty ns lists across all namespaces.
func (c *Client) resourceInterface(gvr schema.GroupVersionResource, namespaced bool, ns string) (dynamic.ResourceInterface, error) {
	dc, err := c.dynamicClient()
	if err != nil {
		return nil, err
	}
	if namespaced && ns != "" {
		return dc.Resource(gvr).Namespace(ns), nil
	}
	return dc.Resource(gvr), nil
}

// ListDynamic lists any resource by GVR via the dynamic client, flattened to
// DynRows sorted by (namespace, name).
func (c *Client) ListDynamic(ctx context.Context, gvr schema.GroupVersionResource, namespaced bool, ns string) ([]DynRow, error) {
	ri, err := c.resourceInterface(gvr, namespaced, ns)
	if err != nil {
		return nil, err
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]DynRow, 0, len(list.Items))
	for i := range list.Items {
		it := &list.Items[i]
		out = append(out, DynRow{
			Namespace: it.GetNamespace(),
			Name:      it.GetName(),
			Age:       humanAge(it.GetCreationTimestamp().Time),
		})
	}
	sortByNsName(len(out),
		func(i int) (string, string) { return out[i].Namespace, out[i].Name },
		func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// DescribeDynamic returns a YAML dump of a dynamically-fetched object plus its
// recent events — the read-only detail for generic/CRD views.
func (c *Client) DescribeDynamic(ctx context.Context, gvr schema.GroupVersionResource, namespaced bool, ns, name string) (string, error) {
	ri, err := c.resourceInterface(gvr, namespaced, ns)
	if err != nil {
		return "", err
	}
	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
	yml, err := yaml.Marshal(obj.Object)
	if err != nil {
		return "", err
	}
	events, err := c.objectEvents(ctx, ns, name)
	if err != nil {
		events = fmt.Sprintf("(failed to load events: %v)", err)
	}
	return fmt.Sprintf("%s\n--- Events ---\n%s", string(yml), events), nil
}
