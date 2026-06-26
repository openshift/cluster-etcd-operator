package ceohelpers

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// Control plane node label selectors
const (
	// ControlPlaneNodeLabelSelector is the label used to identify control plane nodes
	ControlPlaneNodeLabelSelector = "node-role.kubernetes.io/control-plane"

	// ArbiterNodeLabelSelector is the label used to identify arbiter nodes in SNO+1 configurations
	ArbiterNodeLabelSelector = "node-role.kubernetes.io/arbiter"
)

// ListNodesFromInformer returns all nodes from the given informer.
// This is a convenience wrapper around creating a lister and listing all nodes.
// Note: The informer is typically pre-filtered (e.g., controlPlaneNodeInformer),
// so this returns only the nodes that match the informer's filter.
func ListNodesFromInformer(informer cache.SharedIndexInformer) ([]*corev1.Node, error) {
	if informer == nil {
		return nil, fmt.Errorf("informer is nil")
	}

	lister := corev1listers.NewNodeLister(informer.GetIndexer())
	return lister.List(labels.Everything())
}
