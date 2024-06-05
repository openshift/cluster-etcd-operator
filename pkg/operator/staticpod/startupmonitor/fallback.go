package startupmonitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	operatorv1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/staticpod/startupmonitor/annotations"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
)

// fallback implements falling back to the previous version in case the current version
// not ready with a given timeout.
type fallback interface {
	fallbackToPreviousRevision(reason, message string) error
	markRevisionGood(context.Context) error
}

// staticPodFallback implement fallback using static pod last-known-good symlink.
type staticPodFallback struct {
	// revision at which the monitor was started
	revision int

	// targetName hold the name of the operand
	// used to construct the final file name when reading the current and previous manifests
	targetName string

	// manifestsPath points to the directory that holds the root manifests
	manifestsPath string

	// staticPodResourcesPath points to the directory that holds all files supporting the static pod manifests
	staticPodResourcesPath string

	// io collects file system level operations that need to be mocked out during tests
	io ioInterface

	// operatorClient is used to acknowledge readiness.
	operatorClient operatorv1client.KubeAPIServerInterface

	// nodeName is the node hostname as used by the static pod operator resource.
	nodeName string
}

var _ fallback = &staticPodFallback{}

func newStaticPodFallback() *staticPodFallback {
	return &staticPodFallback{io: realFS{}}
}

// TODO: pruner|installer: protect the linked revision
func (f *staticPodFallback) fallbackToPreviousRevision(reason, message string) error {
	klog.Infof("Falling back to a previous revision, the target %v hasn't become ready in the allotted time", f.targetName)
	// step 0: if the last known good revision doesn't exist
	//         find latest previous revision to work with
	//         return in case no revision has been found
	//
	//         note that fileExists method checks the target file not the symlink
	lastKnownExists, err := f.fileExists(f.lastKnownGoodManifestDstPath())
	if err != nil {
		return err
	}
	if !lastKnownExists {
		klog.Info("Have not found the last-known-good manifest, searching for a previous revision's manifest")
		prevRev, found, err := f.findPreviousRevision()
		if err != nil {
			return err
		}
		if !found {
			klog.Infof("Unable to roll back because no previous revision hasn't been found for %s", f.targetName)
			// here we didn't find a previous revision. Not much we can do other than timing out and suicide, leaving the failing pod alone
			return nil
		}

		targetManifestForPrevRevExists, err := f.fileExists(f.targetManifestPathFor(prevRev))
		if err != nil {
			return err // retry, a transient err
		}
		if !targetManifestForPrevRevExists {
			klog.Infof("Unable to roll back because a manifest %q hasn't been found for the previous revision %d", f.targetManifestPathFor(prevRev), prevRev)
			// here we didn't find a previous revision. Not much we can do other than timing out and suicide, leaving the failing pod alone
			return nil
		}

		// step 1: create the last known good revision file
		if err := f.createLastKnowGoodRevisionFor(prevRev); err != nil {
			return err
		}
	}

	// step 2:
	//
	// If the last known good revision exists and we got here that could mean one of:
	// - the current revision is broken
	// - we just created the last known good revision file
	// - the previous iteration of the isReady loop returned an error
	//
	// In that case just:
	// - annotate the manifest
	// - copy the last known good revision manifest (= start the old revision pod)
	lastKnownGoodPod, err := readPod(f.io, f.lastKnownGoodManifestDstPath())
	if err != nil {
		return err
	}
	if lastKnownGoodPod.Annotations == nil {
		lastKnownGoodPod.Annotations = map[string]string{}
	}
	lastKnownGoodPod.Annotations[annotations.FallbackForRevision] = fmt.Sprintf("%d", f.revision)
	lastKnownGoodPod.Annotations[annotations.FallbackReason] = reason
	lastKnownGoodPod.Annotations[annotations.FallbackMessage] = message

	// the kubelet has a bug that prevents graceful termination from working on static pods with the same name, filename
	// and uuid.  By setting the pod UID we can work around the kubelet bug and get our graceful termination honored.
	// Per the node team, this is hard to fix in the kubelet, though it will affect all static pods.
	lastKnownGoodPod.UID = uuid.NewUUID()

	if lastKnownGoodPod.Labels != nil {
		lastKnownGoodPodRevision := lastKnownGoodPod.Labels["revision"]
		klog.Infof("About to fallback to the last-known-good revision %v", lastKnownGoodPodRevision)
	}

	// remove the existing file to ensure kubelet gets "create" event from inotify watchers
	rootTargetManifestPath := filepath.Join(f.manifestsPath, fmt.Sprintf("%s-pod.yaml", f.targetName))
	if err := f.io.Remove(rootTargetManifestPath); err == nil {
		klog.Infof("Removed existing static pod manifest %q", filepath.Join(rootTargetManifestPath))
	} else if !os.IsNotExist(err) {
		return err
	}

	lastKnownGoodPodBytes := []byte(resourceread.WritePodV1OrDie(lastKnownGoodPod))
	klog.Infof("Writing a static pod manifest %q\n\n%s", filepath.Join(rootTargetManifestPath), lastKnownGoodPodBytes)
	if err := f.io.WriteFile(filepath.Join(rootTargetManifestPath), lastKnownGoodPodBytes, 0644); err != nil {
		return err
	}

	return nil
}

func (f *staticPodFallback) markRevisionGood(ctx context.Context) error {
	if err := f.createLastKnowGoodRevisionFor(f.revision); err != nil {
		return err
	}

	// the startup-monitor is authorative to signal readiness of a revision. The operator
	// is waiting for this to happen. Otherwise, the operator could assume readiness while
	// the startup-monitor falls back, leading to an awkward situation.
	// Note that this will retry forever, with a backoff.
	return retry.OnError(retry.DefaultRetry, func(error) bool { return true }, func() error {
		o, err := f.operatorClient.Get(ctx, "cluster", metav1.GetOptions{})
		if err != nil {
			klog.Errorf("Failed to get static pod operator status.nodeStatus: %v", err)
			return err
		}

		for i := range o.Status.NodeStatuses {
			s := &o.Status.NodeStatuses[i]
			if s.NodeName != f.nodeName {
				continue
			}

			if int(s.TargetRevision) != f.revision {
				// target revision mismatch
				return nil
			}

			s.CurrentRevision = s.TargetRevision
			s.TargetRevision = 0

			_, err := f.operatorClient.UpdateStatus(ctx, o, metav1.UpdateOptions{})
			if err != nil {
				klog.Errorf("Failed to update static pod operator status.nodeStatus: %v", err)
				return err
			}
			klog.Infof("Successfully updated node %v to revision %v", s.NodeName, s.CurrentRevision)
		}

		return nil
	})
}

func (f *staticPodFallback) createLastKnowGoodRevisionFor(revision int) error {
	var revisionedTargetManifestPath = f.targetManifestPathFor(revision)

	// step 0: always try to delete the symlink otherwise creation will fail
	//         note that deleting the symlink won't delete the target file
	if err := f.io.Remove(f.lastKnownGoodManifestDstPath()); err != nil && !os.IsNotExist(err) {
		return err
	} else if !os.IsNotExist(err) {
		klog.Infof("Removed existing last known good revision manifest %s", f.lastKnownGoodManifestDstPath())
	}

	// step 1: create last known good revision
	if err := f.io.Symlink(revisionedTargetManifestPath, f.lastKnownGoodManifestDstPath()); err != nil {
		return fmt.Errorf("failed to create a symbolic link %q for %q: %v", f.lastKnownGoodManifestDstPath(), revisionedTargetManifestPath, err)
	}
	klog.Infof("Created a symlink %s for %s", f.lastKnownGoodManifestDstPath(), revisionedTargetManifestPath)
	return nil
}

func (f *staticPodFallback) findPreviousRevision() (int, bool, error) {
	files, err := f.io.ReadDir(f.staticPodResourcesPath)
	if err != nil {
		return 0, false, err
	}

	var nonCurrentRevisions []int
	for _, file := range files {
		if !file.IsDir() {
			continue
		}
		if !strings.HasPrefix(file.Name(), f.targetName+"-pod-") {
			continue
		}

		klog.V(4).Infof("Considering %s for revision extraction", file.Name())
		// now split the file name to get just the revision
		fileSplit := strings.Split(file.Name(), f.targetName+"-pod-")
		if len(fileSplit) != 2 {
			klog.Warningf("Skipping misnamed revision dir %q", file.Name())
			continue
		}
		revision, err := strconv.Atoi(fileSplit[1])
		if err != nil {
			klog.Warningf("Skipping misnamed revision dir %q: %v", file.Name(), err)
			continue
		}

		if revision >= f.revision {
			continue
		}

		targetManifestForRevExists, err := f.fileExists(f.targetManifestPathFor(revision))
		if err != nil {
			klog.Warningf("Skipping %v dir: %v", f.targetManifestPathFor(revision), err)
			continue
		}
		if !targetManifestForRevExists {
			klog.Warningf("Skipping %v because it doesn't contain a manifest file", f.targetManifestPathFor(revision))
			continue
		}

		nonCurrentRevisions = append(nonCurrentRevisions, revision)
	}

	if len(nonCurrentRevisions) == 0 {
		return 0, false, nil
	}
	sort.IntSlice(nonCurrentRevisions).Sort()
	return nonCurrentRevisions[len(nonCurrentRevisions)-1], true, nil
}

func (f *staticPodFallback) fileExists(filepath string) (bool, error) {
	fileInfo, err := f.io.Stat(filepath)
	if err == nil {
		if fileInfo.IsDir() {
			return false, fmt.Errorf("the provided path %v is incorrect and points to a directory", filepath)
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}

	return false, nil
}

func (f *staticPodFallback) lastKnownGoodManifestDstPath() string {
	return filepath.Join(f.staticPodResourcesPath, fmt.Sprintf("%s-last-known-good", f.targetName))
}

func (f *staticPodFallback) targetManifestPathFor(revision int) string {
	return filepath.Join(f.staticPodResourcesPath, fmt.Sprintf("%s-pod-%d", f.targetName, revision), fmt.Sprintf("%s-pod.yaml", f.targetName))
}
