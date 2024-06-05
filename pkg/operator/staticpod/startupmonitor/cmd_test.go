package startupmonitor

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func Test_run(t *testing.T) {
	tests := []struct {
		name string

		ctxDone       bool
		installerLock *fakeLock
		m             *fakeMonitor
		fb            *fakeFallback
		s             *fakeSuicider

		wantErr, wantFallback, wantMarkAsGood, wantSuicide bool
		wantLockTaken                                      bool
	}{
		{
			name:          "run failure",
			installerLock: &fakeLock{},
			m:             &fakeMonitor{result: fakeMonitorRunResult{err: fmt.Errorf("runError")}},
			fb:            &fakeFallback{},
			s:             &fakeSuicider{},

			wantErr: true,
		},
		{
			name:          "run success, not ready",
			installerLock: &fakeLock{},
			m: &fakeMonitor{result: fakeMonitorRunResult{
				ready:   false,
				reason:  "SomeReason",
				message: "SomeMessage",
				err:     nil,
			}},
			fb: &fakeFallback{},
			s:  &fakeSuicider{},

			wantFallback:  true,
			wantSuicide:   true,
			wantLockTaken: true,
		},
		{
			name:          "run success, ready",
			installerLock: &fakeLock{},
			m: &fakeMonitor{result: fakeMonitorRunResult{
				ready:   true,
				reason:  "SomeReason",
				message: "SomeMessage",
				err:     nil,
			}},
			fb: &fakeFallback{},
			s:  &fakeSuicider{},

			wantMarkAsGood: true,
			wantSuicide:    true,
			wantLockTaken:  true,
		},
		{
			name: "lock error",
			installerLock: &fakeLock{
				resultLockError: fmt.Errorf("LockFail"),
			},
			m: &fakeMonitor{result: fakeMonitorRunResult{
				ready:   true,
				reason:  "SomeReason",
				message: "SomeMessage",
				err:     nil,
			}},
			fb: &fakeFallback{},
			s:  &fakeSuicider{},

			wantErr: true,
		},
		{
			name:          "context done",
			installerLock: &fakeLock{},
			m:             &fakeMonitor{result: fakeMonitorRunResult{}},
			fb:            &fakeFallback{},
			s:             &fakeSuicider{},
			ctxDone:       true,

			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancelFn := context.WithCancel(context.Background())
			if tt.ctxDone {
				cancelFn()
			} else {
				defer cancelFn()
			}

			err := run(ctx, tt.installerLock, tt.m, tt.fb, tt.s)
			if err != nil && !tt.wantErr {
				t.Fatalf("expected no run error, but got: %v", err)
			}
			if err == nil && tt.wantErr {
				t.Fatal("expected run error, but got none")
			}

			if tt.wantFallback && tt.fb.got.fallbackToPreviousRevision == nil {
				t.Error("expected fallbackToPreviousRevision call, but didn't observe it")
			} else if !tt.wantFallback && tt.fb.got.fallbackToPreviousRevision != nil {
				t.Errorf("unexpected fallbackToPreviousRevision call: %v", tt.fb.got.fallbackToPreviousRevision)
			}

			if tt.wantMarkAsGood && tt.fb.got.markRevisionGood == nil {
				t.Error("expected markRevisionGood call, but didn't observe it")
			} else if !tt.wantMarkAsGood && tt.fb.got.markRevisionGood != nil {
				t.Errorf("unexpected markRevisionGood call: %v", tt.fb.got.markRevisionGood)
			}

			if tt.wantSuicide && !tt.s.called {
				t.Error("expected suicide, but didn't observe it")
			} else if !tt.wantSuicide && tt.s.called {
				t.Error("unexpected suicide")
			}

			if tt.installerLock.locked && !tt.wantLockTaken {
				t.Error("unexpected taken lock")
			} else if !tt.installerLock.locked && tt.wantLockTaken {
				t.Error("unexpected lock not taken")
			}
		})
	}
}

type fakeLock struct {
	resultLockError   error
	resultUnlockError error

	lock   sync.Mutex
	locked bool

	t testing.T
}

func (f *fakeLock) Lock(ctx context.Context) error {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.resultLockError == nil {
		if f.locked {
			f.t.Errorf("lock already locked")
		}
		f.locked = true
	}

	return f.resultLockError
}

func (f *fakeLock) Unlock() error {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.resultUnlockError == nil {
		if !f.locked {
			f.t.Errorf("lock not locked")
		}
		f.locked = false
	}

	return f.resultUnlockError
}

type fakeMonitorRunGot struct {
	ctx           context.Context
	installerLock Locker
}

type fakeMonitorRunResult struct {
	ready           bool
	reason, message string
	err             error
}

type fakeMonitor struct {
	got    *fakeMonitorRunGot
	result fakeMonitorRunResult
}

func (f *fakeMonitor) Run(ctx context.Context, installerLock Locker) (ready bool, reason string, message string, err error) {
	f.got = &fakeMonitorRunGot{ctx: ctx, installerLock: installerLock}

	// implement the spec about the lock
	if err := installerLock.Lock(ctx); err != nil {
		return false, "", "", err
	}
	select {
	case <-ctx.Done():
		installerLock.Unlock()
		return false, "", "", ctx.Err()
	default:
		if f.result.err != nil {
			installerLock.Unlock()
		}
	}

	return f.result.ready, f.result.reason, f.result.message, f.result.err
}

type fallbackToPreviousRevisionGot struct {
	reason, message string
}

type fakeFallbackResult struct {
	fallbackToPreviousRevision error
	markRevisionGood           error
}

type fakeFallback struct {
	got struct {
		fallbackToPreviousRevision *fallbackToPreviousRevisionGot
		markRevisionGood           *struct{}
	}

	result fakeFallbackResult
}

func (f *fakeFallback) fallbackToPreviousRevision(reason, message string) error {
	f.got.fallbackToPreviousRevision = &fallbackToPreviousRevisionGot{reason: reason, message: message}
	return f.result.fallbackToPreviousRevision
}

func (f *fakeFallback) markRevisionGood(ctx context.Context) error {
	f.got.markRevisionGood = &struct{}{}
	return f.result.markRevisionGood
}

type fakeSuicider struct {
	called bool
}

func (f *fakeSuicider) suicide(installerLock Locker) {
	f.called = true
}
