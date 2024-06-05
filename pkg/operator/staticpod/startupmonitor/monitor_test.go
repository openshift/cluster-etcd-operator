package startupmonitor

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMonitorRun(t *testing.T) {
	scenarios := []struct {
		name          string
		readiness     ReadinessFunc
		installerLock *fakeLock
		validateFn    func(t *testing.T, installerLock *fakeLock, ready bool, reason string, message string, err error)
	}{
		{
			name:          "lock is released on error",
			installerLock: &fakeLock{},
			readiness: func(ctx context.Context, revision int) (ready bool, reason string, message string, err error) {
				return false, "", "", fmt.Errorf("fake err")
			},
			validateFn: func(t *testing.T, installerLock *fakeLock, ready bool, reason string, message string, err error) {
				if ready != false || len(reason) > 0 || len(message) > 0 {
					t.Fatalf("expected ready = false, empty reason and message, got ready = %v, reason = %v, message = %v", ready, reason, message)
				}
				if err == nil || err.Error() != "fake err" {
					t.Fatalf("unexpected err = %v, expected \"fake err\"", err)
				}
				if installerLock.locked {
					t.Fatal("the lock wasn't released")
				}
			},
		},

		{
			name:          "lock is kept when target ready",
			installerLock: &fakeLock{},
			readiness: func(ctx context.Context, revision int) (ready bool, reason string, message string, err error) {
				return true, "", "", nil
			},
			validateFn: func(t *testing.T, installerLock *fakeLock, ready bool, reason string, message string, err error) {
				if ready != true || len(reason) > 0 || len(message) > 0 {
					t.Fatalf("expected ready = false, empty reason and message, got ready = %v, reason = %v, message = %v", ready, reason, message)
				}
				if err != nil {
					t.Fatalf("unexpected err = %v, expected \"fake err\"", err)
				}
				if !installerLock.locked {
					t.Fatal("the lock was released")
				}
			},
		},

		{
			name:          "lock is kept when target unready",
			installerLock: &fakeLock{},
			readiness: func(ctx context.Context, revision int) (ready bool, reason string, message string, err error) {
				return false, "", "", nil
			},
			validateFn: func(t *testing.T, installerLock *fakeLock, ready bool, reason string, message string, err error) {
				if ready != false || len(reason) > 0 || len(message) > 0 {
					t.Fatalf("expected ready = false, empty reason and message, got ready = %v, reason = %v, message = %v", ready, reason, message)
				}
				if err != nil {
					t.Fatalf("unexpected err = %v, expected \"fake err\"", err)
				}
				if !installerLock.locked {
					t.Fatal("the lock was released")
				}
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			target := newMonitor(scenario.readiness)
			target.probeInterval = 100 * time.Millisecond
			target.timeout = 300 * time.Millisecond
			target.manifestsPath = "./testdata"
			target.targetName = "scenario-1"
			target.revision = 8

			// act
			ready, reason, message, err := target.Run(context.TODO(), scenario.installerLock)

			// validate
			scenario.validateFn(t, scenario.installerLock, ready, reason, message, err)
		})
	}
}

func TestLoadTargetManifestAndExtractRevision(t *testing.T) {
	scenarios := []struct {
		name             string
		goldenFilePrefix string
		expectedRev      int
		expectError      bool
	}{

		// scenario 1
		{
			name:             "happy path: a revision is extracted",
			goldenFilePrefix: "scenario-1",
			expectedRev:      8,
		},

		// scenario 2
		{
			name:             "the target pod doesn't have a revision label",
			goldenFilePrefix: "scenario-2",
			expectError:      true,
		},

		// scenario 3
		{
			name:             "the target pod has an incorrect label",
			goldenFilePrefix: "scenario-3",
			expectError:      true,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			target := newMonitor(nil)
			target.manifestsPath = "./testdata"
			target.targetName = scenario.goldenFilePrefix

			// act
			rev, err := target.loadRootTargetPodAndExtractRevision()

			// validate
			if err != nil && !scenario.expectError {
				t.Fatal(err)
			}
			if err == nil && scenario.expectError {
				t.Fatal("expected to get an error")
			}
			if rev != scenario.expectedRev {
				t.Errorf("unexpected rev %d, expected %d", rev, scenario.expectedRev)
			}
		})
	}
}

func validateError(t *testing.T, actualErr error, expectedErr string) {
	if actualErr != nil && len(expectedErr) == 0 {
		t.Fatalf("unexpected error: %v", actualErr)
	}
	if actualErr == nil && len(expectedErr) > 0 {
		t.Fatal("expected to get an error")
	}
	if actualErr != nil && actualErr.Error() != expectedErr {
		t.Fatalf("incorrect error: %v, expected: %v", actualErr, expectedErr)
	}
}
