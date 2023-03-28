package health

import (
	"context"
	"time"

	"github.com/openshift/library-go/pkg/controller/factory"
)

// CheckingSyncWrapper wraps calls to the factory.SyncFunc in order to track when it last ran successfully.
type CheckingSyncWrapper struct {
	lastSuccessfulRun time.Time
	syncFunc          factory.SyncFunc
	livenessThreshold time.Duration
}

func (r *CheckingSyncWrapper) Sync(ctx context.Context, controllerContext factory.SyncContext) error {
	err := r.syncFunc(ctx, controllerContext)
	if err == nil {
		r.lastSuccessfulRun = time.Now()
	}
	return err
}

func (r *CheckingSyncWrapper) Alive() bool {
	return r.lastSuccessfulRun.Add(r.livenessThreshold).After(time.Now())
}

func NewCheckingSyncWrapper(sync factory.SyncFunc, livenessThreshold time.Duration) *CheckingSyncWrapper {
	return &CheckingSyncWrapper{
		lastSuccessfulRun: time.Now(),
		syncFunc:          sync,
		livenessThreshold: livenessThreshold,
	}
}
