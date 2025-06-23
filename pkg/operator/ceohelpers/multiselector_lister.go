package ceohelpers

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	cache "k8s.io/client-go/tools/cache"
)

// NewMultiSelectorNodeLister create a node lister that allows for querying based on multiple selectors for ORing on keys.
// This is done because of a performance design choice in selector queries that does not allow for ORing on keys.
// see: https://github.com/kubernetes/kubernetes/issues/90549#issuecomment-620625847
//
// NOTE: If you are selecting based on ORing values, that is already supported, please use a format like `key in (value1,value2)`
// see: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#set-based-requirement
func NewMultiSelectorNodeLister(indexer cache.Indexer, selectors ...labels.Selector) corev1listers.NodeLister {
	return &mergedNodeLister{indexer: indexer, selectorArray: selectors}
}

type mergedNodeLister struct {
	indexer       cache.Indexer
	selectorArray []labels.Selector
}

// List lists all Nodes in the indexer.
func (s *mergedNodeLister) List(selector labels.Selector) (ret []*corev1.Node, err error) {
	var (
		isSelectorPartOfList bool
		nodeList             []*corev1.Node
	)
	for _, labelSelector := range s.selectorArray {
		if labelSelector.String() == selector.String() {
			isSelectorPartOfList = true
		}
		err = cache.ListAll(s.indexer, labelSelector, func(m interface{}) {
			nodeList = append(nodeList, m.(*corev1.Node))
		})
	}
	// If desired selector is part of selector array, we are done and return nodes
	if isSelectorPartOfList {
		return nodeList, err
	}
	// If selector is not part of main list, we look through nodes and trim more
	for _, node := range nodeList {
		nodeLabels := labels.Set(node.GetLabels())
		if selector.Matches(nodeLabels) {
			ret = append(ret, node)
		}
	}
	return ret, err
}

// Get retrieves the Node from the index for a given name.
func (s *mergedNodeLister) Get(name string) (*corev1.Node, error) {
	obj, exists, err := s.indexer.GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(corev1.Resource("node"), name)
	}
	return obj.(*corev1.Node), nil
}
