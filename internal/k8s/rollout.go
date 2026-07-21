package k8s

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// revisionAnnotation is where the Deployment controller records a ReplicaSet's
// revision number.
const revisionAnnotation = "deployment.kubernetes.io/revision"

// RolloutUndo rolls a workload back to its previous revision, mirroring
// `kubectl rollout undo`. There is no server-side rollback endpoint, so this is
// reconstructed client-side: Deployments swap in the prior ReplicaSet's pod
// template; StatefulSets/DaemonSets re-apply the prior ControllerRevision patch.
// It returns a human-readable result message.
func (c *Client) RolloutUndo(ctx context.Context, kind, namespace, name string) (string, error) {
	switch kind {
	case "deployment":
		return c.undoDeployment(ctx, namespace, name)
	case "statefulset":
		return c.undoControllerRevision(ctx, "statefulset", namespace, name)
	case "daemonset":
		return c.undoControllerRevision(ctx, "daemonset", namespace, name)
	default:
		return "", fmt.Errorf("cannot roll back kind %q", kind)
	}
}

// undoDeployment finds the ReplicaSet one revision below the current and patches
// the Deployment's pod template back to it (stripping the controller-managed
// pod-template-hash label the ReplicaSet carries).
func (c *Client) undoDeployment(ctx context.Context, namespace, name string) (string, error) {
	deps := c.clientset.AppsV1().Deployments(namespace)
	dep, err := deps.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return "", err
	}
	rsList, err := c.clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return "", err
	}

	// Collect this Deployment's ReplicaSets with a parseable revision.
	type rev struct {
		num int64
		rs  *appsv1.ReplicaSet
	}
	var revs []rev
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if !ownedBy(rs.OwnerReferences, dep.UID) {
			continue
		}
		n, err := strconv.ParseInt(rs.Annotations[revisionAnnotation], 10, 64)
		if err != nil {
			continue
		}
		revs = append(revs, rev{num: n, rs: rs})
	}
	if len(revs) < 2 {
		return "", fmt.Errorf("no previous revision to roll back to")
	}
	sort.Slice(revs, func(i, j int) bool { return revs[i].num > revs[j].num }) // newest first

	current, _ := strconv.ParseInt(dep.Annotations[revisionAnnotation], 10, 64)
	var target *rev
	for i := range revs {
		if revs[i].num < current {
			target = &revs[i]
			break
		}
	}
	if target == nil {
		return "", fmt.Errorf("no previous revision to roll back to")
	}

	template := target.rs.Spec.Template.DeepCopy()
	delete(template.Labels, "pod-template-hash") // hash is added by the controller, not part of the template
	dep.Spec.Template = *template
	if _, err := deps.Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return "", err
	}
	return fmt.Sprintf("rolled back deployment/%s to revision %d", name, target.num), nil
}

// undoControllerRevision re-applies the ControllerRevision one below the current
// as a strategic-merge patch, the mechanism StatefulSets and DaemonSets use.
func (c *Client) undoControllerRevision(ctx context.Context, kind, namespace, name string) (string, error) {
	var (
		sel *metav1.LabelSelector
		uid types.UID
	)
	switch kind {
	case "statefulset":
		obj, err := c.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		sel, uid = obj.Spec.Selector, obj.UID
	case "daemonset":
		obj, err := c.clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		sel, uid = obj.Spec.Selector, obj.UID
	default:
		return "", fmt.Errorf("cannot roll back kind %q", kind)
	}

	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return "", err
	}
	crList, err := c.clientset.AppsV1().ControllerRevisions(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return "", err
	}
	var owned []appsv1.ControllerRevision
	for i := range crList.Items {
		if ownedBy(crList.Items[i].OwnerReferences, uid) {
			owned = append(owned, crList.Items[i])
		}
	}
	if len(owned) < 2 {
		return "", fmt.Errorf("no previous revision to roll back to")
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Revision > owned[j].Revision }) // newest first
	target := owned[1]                                                                      // one below the current (highest)

	patch := target.Data.Raw
	if len(patch) == 0 {
		return "", fmt.Errorf("revision %d has no stored data", target.Revision)
	}
	switch kind {
	case "statefulset":
		_, err = c.clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	case "daemonset":
		_, err = c.clientset.AppsV1().DaemonSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("rolled back %s/%s to revision %d", kind, name, target.Revision), nil
}

// ownedBy reports whether any owner reference points at uid.
func ownedBy(owners []metav1.OwnerReference, uid types.UID) bool {
	for _, o := range owners {
		if o.UID == uid {
			return true
		}
	}
	return false
}
