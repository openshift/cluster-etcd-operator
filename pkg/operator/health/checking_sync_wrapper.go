package health

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/openshift/library-go/pkg/controller/factory"
)

// CheckingSyncWrapper wraps calls to the factory.SyncFunc in order to track when it last ran successfully.
type CheckingSyncWrapper struct {
	syncFunc          factory.SyncFunc
	livenessThreshold time.Duration

	// lastSuccessfulRun is updated with atomics, as updates can come from different goroutines
	lastSuccessfulRun int64
}

func (r *CheckingSyncWrapper) Sync(ctx context.Context, controllerContext factory.SyncContext) error {
	err := r.syncFunc(ctx, controllerContext)
	if err == nil {
		atomic.StoreInt64(&r.lastSuccessfulRun, time.Now().UnixMilli())
	}
	return err
}

func (r *CheckingSyncWrapper) Alive() bool {
	lastRun := time.UnixMilli(atomic.LoadInt64(&r.lastSuccessfulRun))
	return lastRun.Add(r.livenessThreshold).After(time.Now())
}

func NewCheckingSyncWrapper(sync factory.SyncFunc, livenessThreshold time.Duration) *CheckingSyncWrapper {
	return &CheckingSyncWrapper{
		lastSuccessfulRun: time.Now().UnixMilli(),
		syncFunc:          sync,
		livenessThreshold: livenessThreshold,
	}
}
