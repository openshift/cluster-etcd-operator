package testutil

import (
	"context"
	"fmt"
	"time"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"k8s.io/client-go/tools/cache"
)

func CreateFakeInformer(obj *pacmkrv1.PacemakerCluster) cache.SharedIndexInformer {
	store := cache.NewStore(func(obj any) (string, error) {
		if pc, ok := obj.(*pacmkrv1.PacemakerCluster); ok {
			return pc.Name, nil
		}
		return "", fmt.Errorf("object is not a PacemakerCluster")
	})
	if obj != nil {
		_ = store.Add(obj)
	}
	return &fakeInformer{store: store}
}

type fakeInformer struct {
	store cache.Store
}

func (f *fakeInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (f *fakeInformer) AddEventHandlerWithResyncPeriod(handler cache.ResourceEventHandler, resyncPeriod time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (f *fakeInformer) AddEventHandlerWithOptions(handler cache.ResourceEventHandler, options cache.HandlerOptions) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (f *fakeInformer) RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error {
	return nil
}
func (f *fakeInformer) GetStore() cache.Store           { return f.store }
func (f *fakeInformer) GetController() cache.Controller { return nil }
func (f *fakeInformer) Run(stopCh <-chan struct{})      {}
func (f *fakeInformer) RunWithContext(ctx context.Context) {
}
func (f *fakeInformer) HasSynced() bool                                            { return true }
func (f *fakeInformer) LastSyncResourceVersion() string                            { return "" }
func (f *fakeInformer) SetWatchErrorHandler(handler cache.WatchErrorHandler) error { return nil }
func (f *fakeInformer) SetWatchErrorHandlerWithContext(handler cache.WatchErrorHandlerWithContext) error {
	return nil
}
func (f *fakeInformer) SetTransform(handler cache.TransformFunc) error { return nil }
func (f *fakeInformer) IsStopped() bool                                { return false }
func (f *fakeInformer) AddIndexers(indexers cache.Indexers) error      { return nil }
func (f *fakeInformer) GetIndexer() cache.Indexer                      { return nil }
