package health

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/stretchr/testify/require"
)

func TestHappyPathAliveness(t *testing.T) {
	testCases := map[string]struct {
		mockSuccessTime time.Time
		threshold       time.Duration
		expectedAlive   bool
	}{
		"just-started": {
			mockSuccessTime: time.Now(),
			threshold:       1 * time.Minute,
			expectedAlive:   true,
		},
		"last-success-long-ago": {
			mockSuccessTime: time.Now().Add(-20 * time.Minute),
			threshold:       5 * time.Minute,
			expectedAlive:   false,
		},
	}

	for testName, tc := range testCases {
		t.Run(testName, func(t *testing.T) {
			syncer := NewCheckingSyncWrapper(func(ctx context.Context, controllerContext factory.SyncContext) error {
				return nil
			}, tc.threshold)
			syncer.lastSuccessfulRun = tc.mockSuccessTime.UnixMilli()
			require.Equal(t, tc.expectedAlive, syncer.Alive())
		})
	}

}

func TestErrorDoesNotUpdateSuccess(t *testing.T) {
	syncer := NewCheckingSyncWrapper(func(ctx context.Context, controllerContext factory.SyncContext) error {
		return fmt.Errorf("some")
	}, 1*time.Second)
	// setting it to zero to ensure no update happened
	syncer.lastSuccessfulRun = 0

	err := syncer.Sync(context.Background(), nil)
	require.Error(t, err)
	require.Equal(t, int64(0), syncer.lastSuccessfulRun)
	require.False(t, syncer.Alive())
}
