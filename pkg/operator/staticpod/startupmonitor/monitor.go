package startupmonitor

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"time"

	"github.com/openshift/library-go/pkg/operator/resource/resourceread"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

type ReadinessFunc func(ctx context.Context, revision int) (ready bool, reason string, message string, err error)

type Locker interface {
	Lock(ctx context.Context) error
	Unlock() error
}

type Monitor interface {
	// Run checks the target for readiness and returns with true if the all readiness
	// checks return true within the given timeout duration, or false if the timeout
	// has passed. It will return an error if the context is done before the timeout
	// expired.
	//
	// When Run returns without error, the lock is kept (and must be released by the caller.
	// In the case it does neither timeout nor the target gets ready, the lock is released.
	//
	// installerLock blocks the installer from running in parallel. The monitor will run
	// every iteration of the probe interval with this lock taken. When Run returns
	// without error, the lock is kept (and must be released by the caller.
	// In the case it does neither timeout nor the target gets ready, the lock is released.
	Run(ctx context.Context, installerLock Locker) (ready bool, reason string, message string, err error)
}

// monitor watches an operand's readiness condition
//
// This monitor understands a directory structure created by an OCP installation. That is:
//
//	The root manifest are looked up in the manifestPath
//	The revisioned manifest are looked up in the staticPodResourcesPath
//	The target (operand) name is derived from the targetName.
type monitor struct {
	// probeInterval specifies a time interval at which health of the target will be assessed
	// be mindful of not setting it too low, on each iteration, an i/o is involved
	probeInterval time.Duration

	// timeout specifies the time the monitor will attempt to wait for readiness
	timeout time.Duration

	// revision at which the monitor was started
	revision int

	// targetName hold the name of the operand
	// used to construct the final file name when reading the current and previous manifests
	targetName string

	// manifestsPath points to the directory that holds the root manifests
	manifestsPath string

	// check is the readiness check funcs called in order until the first fails.
	check ReadinessFunc

	// io collects file system level operations that need to be mocked out during tests
	io ioInterface
}

var _ Monitor = &monitor{}

func newMonitor(isReady ReadinessFunc) *monitor {
	return &monitor{check: isReady, io: realFS{}}
}

func (m *monitor) Run(ctx context.Context, installerLock Locker) (ready bool, reason string, message string, err error) {
	klog.Infof("Waiting for readiness (interval %v, timeout %v)...", m.probeInterval, m.timeout)

	lastReady := false
	var lastError error
	var lastReason, lastMessage string
	iteration := func(ctx context.Context) {
		// acquire an exclusive lock to coordinate work with the installer pod, e.g.:
		//
		// an installer is in progress and wants to install a new revision
		// the current revision is not healthy and we are about to fall back to the previous version (fallbackToPreviousRevision method)
		// the installer writes the new file and we immediately overwrite it
		//
		// additional benefit is that we read consistent operand's manifest
		if lastError = installerLock.Lock(ctx); lastError != nil {
			klog.Error(lastError)
			return
		}

		lastReady, lastReason, lastMessage, lastError = m.isReady(ctx)
		if lastError != nil {
			klog.Error(lastError)
			return
		}
		if len(lastReason) > 0 {
			klog.Infof("Watching %s of revision %d: %s (%s)", m.targetName, m.revision, lastMessage, lastReason)
		}
	}

	// we use wait.Until because it uses sliding interval, wait.Poll does not.
	withTimeoutCtx, cancel := context.WithTimeout(ctx, m.timeout-m.probeInterval) // last iteration is done manually to have control over the Unlock
	wait.UntilWithContext(withTimeoutCtx, func(ctx context.Context) {
		defer func() {
			if !lastReady {
				installerLock.Unlock()
			}
		}()
		iteration(ctx)
		if lastReady {
			cancel()
		}
	}, m.probeInterval)

	// keep the lock (for suicide) and return
	if lastReady {
		return true, "", "", nil
	}

	// at this point we are either not ready or we got an error
	// do last iteration without calling Unlock()
	iteration(ctx)
	if lastError != nil {
		// release the lock since we are exiting anyway
		installerLock.Unlock()
		return false, "", "", lastError
	}

	// outer context done is different, as this will likely be from a signal.
	select {
	case <-ctx.Done():
		installerLock.Unlock()
		return false, "", "", ctx.Err()
	default:
	}

	return lastReady, lastReason, lastMessage, nil
}

func (m *monitor) isReady(ctx context.Context) (ready bool, reason string, message string, err error) {
	// to avoid issues on startup and downgrade (before the startup monitor was introduced check the current target's revision.
	// refrain from any further processing in case we have a mismatch.
	currentTargetRevision, err := m.loadRootTargetPodAndExtractRevision()
	if err != nil {
		return false, "", "", err
	}
	if m.revision != currentTargetRevision {
		// return an error so that we won't fallback when we fail to monitor unexpected target
		return false, "", "", fmt.Errorf("expected revision %d, found %d", m.revision, currentTargetRevision)
	}

	// first check if the target is ready.
	return m.check(ctx, m.revision)
}

// note that there is a fight between the installer pod (writer) and the startup monitor (reader) when dealing with the target manifest file.
// since the monitor is resynced every probeInterval it seems we can deal with an error or stale content
//
// note if this code will return buffered data due to perf reason revisit fallbackToPreviousRevision
// as it currently assumes strong consistency
func (m *monitor) loadRootTargetPodAndExtractRevision() (int, error) {
	currentTargetPod, err := readPod(m.io, path.Join(m.manifestsPath, fmt.Sprintf("%s-pod.yaml", m.targetName)))
	if err != nil {
		return 0, err
	}

	revisionString, found := currentTargetPod.Labels["revision"]
	if !found {
		return 0, fmt.Errorf("pod %s doesn't have revision label", currentTargetPod.Name)
	}
	if len(revisionString) == 0 {
		return 0, fmt.Errorf("empty revision label on %s pod", currentTargetPod.Name)
	}
	revision, err := strconv.Atoi(revisionString)
	if err != nil || revision < 0 {
		return 0, fmt.Errorf("invalid revision label on pod %s: %q", currentTargetPod.Name, revisionString)
	}

	return revision, nil
}

func readPod(io ioInterface, filepath string) (*corev1.Pod, error) {
	rawManifest, err := io.ReadFile(filepath)
	if err != nil {
		return nil, err
	}
	currentTargetPod, err := resourceread.ReadPodV1(rawManifest)
	if err != nil {
		return nil, err
	}
	return currentTargetPod, nil
}

type nullMutex struct{}

func (m nullMutex) Lock(ctx context.Context) error {
	return nil
}

func (m nullMutex) Unlock() error {
	return nil
}
