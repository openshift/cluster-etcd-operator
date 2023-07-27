package ceohelpers

import (
	"k8s.io/client-go/tools/cache"
)

// AlreadySyncedInformer wraps an ordinary cache.SharedInformer, but allows us to fake HasSynced results.
// This is to allow controllers to proceed when an informer could not sync (eg when the API isn't available).
type AlreadySyncedInformer struct {
	cache.SharedInformer
}

func NewAlreadySyncedInformer(informer cache.SharedInformer) cache.SharedInformer {
	return &AlreadySyncedInformer{
		SharedInformer: informer,
	}
}

func (s *AlreadySyncedInformer) HasSynced() bool {
	return true
}
