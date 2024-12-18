package ceohelpers

import (
	"context"
	time "time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	kubernetes "k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	cache "k8s.io/client-go/tools/cache"
)

// NewMultiSelectorNodeLister create a node lister that allows for querying based on multiple selectors for ORing on keys.
// This is done because of a performance design choice in selector queries that does not allow for ORing on keys.
// see: https://github.com/kubernetes/kubernetes/issues/90549#issuecomment-620625847
//
// NOTE: If you are selecting based on ORing values, that is already supported, please use a format like `key in (value1,value2)`
// see: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#set-based-requirement
func NewMultiSelectorNodeLister(indexer cache.Indexer, extraSelectors ...labels.Selector) corev1listers.NodeLister {
	return &mergedNodeLister{indexer: indexer, extraSelectors: extraSelectors}
}

// NewMultiSelectorNodeInformer create a node informer with multiple selector types for ORing on the keys of node types.
// This is done because of a performance design choice in selector queries that does not allow for ORing on keys.
// see: https://github.com/kubernetes/kubernetes/issues/90549#issuecomment-620625847
//
// NOTE: If you are selecting based on ORing values, that is already supported, please use a format like `key in (value1,value2)`
// see: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#set-based-requirement
func NewMultiSelectorNodeInformer(client kubernetes.Interface, resyncPeriod time.Duration, indexers cache.Indexers, selectors ...string) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				var filteredNodes *corev1.NodeList
				for _, selector := range selectors {
					options.LabelSelector = selector
					nodes, err := client.CoreV1().Nodes().List(context.TODO(), options)
					if err != nil {
						return nil, err
					}
					if filteredNodes == nil {
						filteredNodes = nodes
					} else {
						filteredNodes.Items = append(filteredNodes.Items, nodes.Items...)
					}
				}
				return filteredNodes, nil
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				var filteredNodeWatchers mergedWatchFunc
				for _, selector := range selectors {
					options.LabelSelector = selector
					nodeWatcher, err := client.CoreV1().Nodes().Watch(context.TODO(), options)
					if err != nil {
						return nil, err
					}
					filteredNodeWatchers = append(filteredNodeWatchers, nodeWatcher)
				}
				return filteredNodeWatchers, nil
			},
		},
		&corev1.Node{},
		resyncPeriod,
		indexers,
	)
}

type mergedNodeLister struct {
	indexer        cache.Indexer
	extraSelectors []labels.Selector
}

// List lists all Nodes in the indexer.
func (s *mergedNodeLister) List(selector labels.Selector) (ret []*corev1.Node, err error) {
	err = cache.ListAll(s.indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*corev1.Node))
	})
	for _, selector := range s.extraSelectors {
		err = cache.ListAll(s.indexer, selector, func(m interface{}) {
			ret = append(ret, m.(*corev1.Node))
		})
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

type mergedWatchFunc []watch.Interface

func (funcs mergedWatchFunc) Stop() {
	for _, fun := range funcs {
		fun.Stop()
	}
}

func (funcs mergedWatchFunc) ResultChan() <-chan watch.Event {
	out := make(chan watch.Event)
	for _, fun := range funcs {
		go func(eventChannel <-chan watch.Event) {
			for v := range eventChannel {
				out <- v
			}
		}(fun.ResultChan())
	}
	return out
}
