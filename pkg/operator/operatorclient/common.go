package operatorclient

import (
	"context"
	"fmt"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"time"
)

func AwaitInformerCacheSync(name string, inf cache.SharedIndexInformer) error {
	klog.Infof("waiting for [%s] informer sync...", name)
	versionTimeoutCtx, versionTimeoutCancelFunc := context.WithTimeout(context.Background(), 5*time.Minute)
	clusterVersionInformerSynced := cache.WaitForCacheSync(versionTimeoutCtx.Done(), inf.HasSynced)
	versionTimeoutCancelFunc()

	if !clusterVersionInformerSynced {
		return fmt.Errorf("could not sync [%s] informer, aborting operator start", name)
	}
	return nil
}
