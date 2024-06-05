package installer

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/staticpod/controller/revision"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/staticpod/startupmonitor/annotations"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/condition"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/events/eventstesting"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	clocktesting "k8s.io/utils/clock/testing"
)

func TestNewNodeStateForInstallInProgress(t *testing.T) {
	before := metav1.NewTime(time.Date(2021, 1, 2, 3, 3, 4, 5, time.UTC))
	now := metav1.NewTime(time.Date(2021, 1, 2, 3, 4, 5, 6, time.UTC))
	//nowString := now().Format(time.RFC3339)

	installer := func(name string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "test",
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Status: corev1.ConditionFalse,
						Type:   corev1.PodReady,
					},
				},
				Phase: phase,
			},
		}
	}
	withMessage := func(p *corev1.Pod, msg string) *corev1.Pod {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "container",
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					Message: msg,
				},
			},
		}}
		p.Status.Message = msg
		return p
	}
	operand := func(name string, revision string, phase corev1.PodPhase, ready bool) *corev1.Pod {
		readyStatus := corev1.ConditionFalse
		if ready {
			readyStatus = corev1.ConditionTrue
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              mirrorPodNameForNode(name, "test-node-1"),
				Namespace:         "test",
				Labels:            map[string]string{"revision": revision},
				CreationTimestamp: metav1Timestamp("15:07:03"),
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Status: readyStatus,
						Type:   corev1.PodReady,
					},
				},
				Phase: phase,
			},
		}
	}
	fallbackOperand := func(name string, lastKnownGoodRevision string, phase corev1.PodPhase, ready bool, creationTimestamp time.Time, failedRevision, fallbackReason, fallbackMessage string) *corev1.Pod {
		pod := operand(name, lastKnownGoodRevision, phase, ready)
		pod.CreationTimestamp = metav1.NewTime(creationTimestamp)
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[annotations.FallbackForRevision] = failedRevision
		if len(fallbackReason) > 0 {
			pod.Annotations[annotations.FallbackReason] = fallbackReason
		}
		if len(fallbackMessage) > 0 {
			pod.Annotations[annotations.FallbackMessage] = fallbackMessage
		}
		return pod
	}

	type Expected struct {
		nodeStatus         *operatorv1.NodeStatus
		installerPodFailed bool
		reason             string
		err                bool
	}
	type Test struct {
		name                    string
		pods                    []*corev1.Pod
		latestAvailableRevision int32
		current                 operatorv1.NodeStatus
		startupMonitorEnabled   bool
		expected                Expected
	}
	for _, test := range []Test{
		{"installer-pending", []*corev1.Pod{
			withMessage(installer("installer-1-test-node-1", corev1.PodPending), "creating containers"),
		}, 1, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:        "test-node-1",
				CurrentRevision: 0,
				TargetRevision:  1,
			},
			false,
			"installer is not finished: creating containers",
			false,
		}},

		{"installer-pending-no-message", []*corev1.Pod{
			installer("installer-1-test-node-1", corev1.PodPending),
		}, 1, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:        "test-node-1",
				CurrentRevision: 0,
				TargetRevision:  1,
			},
			false,
			"installer is not finished, but in Pending phase",
			false,
		}},

		{"installer-running", []*corev1.Pod{
			installer("installer-1-test-node-1", corev1.PodRunning),
		}, 1, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:        "test-node-1",
				CurrentRevision: 0,
				TargetRevision:  1,
			},
			false,
			"installer is not finished, but in Running phase",
			false,
		}},

		{"installer-missing", []*corev1.Pod{}, 1, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:        "test-node-1",
				CurrentRevision: 0,
				TargetRevision:  1,
			},
			true,
			"pod installer-1-test-node-1 disappeared",
			false,
		}},

		{"installer-failed", []*corev1.Pod{
			withMessage(installer("installer-1-test-node-1", corev1.PodFailed), "bla bla OOM bla bla"),
		}, 1, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:                 "test-node-1",
				CurrentRevision:          0,
				TargetRevision:           1,
				LastFailedRevision:       1,
				LastFailedTime:           &now,
				LastFailedCount:          1,
				LastFailedRevisionErrors: []string{"container: bla bla OOM bla bla"},
				LastFailedReason:         "InstallerFailed",
			},
			true,
			"installer pod failed: container: bla bla OOM bla bla",
			false,
		}},

		{"installer-failed-without-message", []*corev1.Pod{
			installer("installer-1-test-node-1", corev1.PodFailed),
		}, 1, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:                 "test-node-1",
				CurrentRevision:          0,
				TargetRevision:           1,
				LastFailedRevision:       1,
				LastFailedTime:           &now,
				LastFailedCount:          1,
				LastFailedRevisionErrors: []string{"no detailed termination message, see `oc get -oyaml -n \"test\" pods \"installer-1-test-node-1\"`"},
				LastFailedReason:         "InstallerFailed",
			},
			true,
			"installer pod failed: no detailed termination message, see `oc get -oyaml -n \"test\" pods \"installer-1-test-node-1\"`",
			false,
		}},

		{"installer-retry-failed", []*corev1.Pod{
			withMessage(installer("installer-1-retry-2-test-node-1", corev1.PodFailed), "bla bla OOM bla bla 2"),
		}, 1, operatorv1.NodeStatus{
			NodeName:                 "test-node-1",
			CurrentRevision:          0,
			TargetRevision:           1,
			LastFailedCount:          2,
			LastFailedRevision:       1,
			LastFailedRevisionErrors: []string{"container: bla bla OOM bla bla"},
			LastFailedTime:           &before,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:                 "test-node-1",
				CurrentRevision:          0,
				TargetRevision:           1,
				LastFailedRevision:       1,
				LastFailedTime:           &now,
				LastFailedCount:          3,
				LastFailedRevisionErrors: []string{"container: bla bla OOM bla bla 2"},
				LastFailedReason:         "InstallerFailed",
			},
			true,
			"installer pod failed: container: bla bla OOM bla bla 2",
			false,
		}},

		{"installer-succeeded-and-operand-missing", []*corev1.Pod{
			installer("installer-1-test-node-1", corev1.PodSucceeded),
		}, 1, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:        "test-node-1",
				CurrentRevision: 0,
				TargetRevision:  1,
			},
			false,
			"static pod is pending",
			false,
		}},

		{"installer-succeeded-and-operand-old", []*corev1.Pod{
			installer("installer-1-test-node-1", corev1.PodSucceeded),
			operand("test-pod", "0", corev1.PodRunning, true),
		}, 1, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:        "test-node-1",
				CurrentRevision: 0,
				TargetRevision:  1,
			},
			false,
			"waiting for static pod of revision 1, found 0",
			false,
		}},

		{"installer-succeeded-and-operand-not-ready", []*corev1.Pod{
			installer("installer-1-test-node-1", corev1.PodSucceeded),
			operand("test-pod", "1", corev1.PodRunning, false),
		}, 1, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:        "test-node-1",
				CurrentRevision: 0,
				TargetRevision:  1,
			},
			false,
			"static pod is pending",
			false,
		}},

		{"installer-succeeded-and-operand-ready", []*corev1.Pod{
			installer("installer-1-test-node-1", corev1.PodSucceeded),
			operand("test-pod", "1", corev1.PodRunning, true),
		}, 1, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:        "test-node-1",
				CurrentRevision: 1,
				TargetRevision:  0,
			},
			false,
			"static pod is ready",
			false,
		}},
		{"installer-succeeded-and-operand-failed", []*corev1.Pod{
			installer("installer-1-test-node-1", corev1.PodSucceeded),
			withMessage(operand("test-pod", "1", corev1.PodFailed, false), "segfault"),
		}, 1, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:                 "test-node-1",
				CurrentRevision:          0,
				TargetRevision:           0,
				LastFailedRevision:       1,
				LastFailedCount:          1,
				LastFailedReason:         "OperandFailed",
				LastFailedTime:           metav1TimestampPtr("15:07:03"),
				LastFailedRevisionErrors: []string{"segfault"},
			},
			false,
			"operand pod failed: segfault",
			false,
		}},

		{"installer-succeeded-and-new-revision-pending", []*corev1.Pod{
			installer("installer-1-test-node-1", corev1.PodSucceeded),
			operand("test-pod", "1", corev1.PodPending, false),
		}, 2, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:        "test-node-1",
				CurrentRevision: 0,
				TargetRevision:  2,
			},
			false,
			"new revision pending",
			false,
		}},

		{"installer-failed-and-new-revision-pending", []*corev1.Pod{
			withMessage(installer("installer-1-test-node-1", corev1.PodFailed), "bla bla OOM bla bla"),
		}, 2, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:        "test-node-1",
				CurrentRevision: 0,
				TargetRevision:  2,
			},
			false,
			"new revision pending",
			false,
		}},

		{"installer-retry-failed-and-new-revision-pending", []*corev1.Pod{
			withMessage(installer("installer-1-retry-2-test-node-1", corev1.PodFailed), "bla bla OOM bla bla 2"),
		}, 2, operatorv1.NodeStatus{
			NodeName:                 "test-node-1",
			CurrentRevision:          0,
			TargetRevision:           1,
			LastFailedRevisionErrors: []string{"container: bla bla OOM bla bla"},
			LastFailedRevision:       1,
			LastFailedCount:          2,
			LastFailedTime:           &before,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:                 "test-node-1",
				CurrentRevision:          0,
				TargetRevision:           2,
				LastFailedRevisionErrors: []string{"container: bla bla OOM bla bla"},
				LastFailedRevision:       1,
				LastFailedCount:          2,
				LastFailedTime:           &before,
			},
			false,
			"new revision pending",
			false,
		}},

		{"installer-pending-and-new-revision-pending", []*corev1.Pod{
			installer("installer-1-test-node-1", corev1.PodPending),
		}, 2, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, false, Expected{
			&operatorv1.NodeStatus{
				NodeName:        "test-node-1",
				CurrentRevision: 0,
				TargetRevision:  2,
			},
			false,
			"new revision pending",
			false,
		}},

		// the fallback cases
		{"installer-succeeded-and-fallback-comes-up", []*corev1.Pod{
			installer("installer-4-test-node-1", corev1.PodSucceeded),
			fallbackOperand("test-pod", "2", corev1.PodRunning, true, timestamp("15:04:01"), "4", "CrashLooping", "pod is crash-looping"),
		}, 4, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 2,
			TargetRevision:  4,
		}, true, Expected{
			&operatorv1.NodeStatus{
				NodeName:                 "test-node-1",
				CurrentRevision:          2,
				TargetRevision:           4,
				LastFailedCount:          0,
				LastFailedRevision:       4,
				LastFailedTime:           metav1TimestampPtr("15:04:01"),
				LastFailedRevisionErrors: []string{"fallback to last-known-good revision 2 took place after: pod is crash-looping (CrashLooping)"},
				LastFailedReason:         "OperandFailedFallback",
				LastFallbackCount:        1,
			},
			false,
			"static pod for 4 did not launch and last-known-good revision 2 has been started",
			false,
		}},
		{"installer-succeeded-and-operand-failed-but-no-fallback-yet", []*corev1.Pod{
			installer("installer-1-test-node-1", corev1.PodSucceeded),
			withMessage(operand("test-pod", "1", corev1.PodFailed, false), "segfault"),
		}, 1, operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 0,
			TargetRevision:  1,
		}, true, Expected{
			&operatorv1.NodeStatus{
				NodeName:                 "test-node-1",
				CurrentRevision:          0,
				TargetRevision:           1,
				LastFailedRevision:       1,
				LastFailedCount:          0, // we don't increase count in startup-monitor mode before fallback
				LastFailedReason:         "OperandFailed",
				LastFailedTime:           nil, // we don't set the timestamp yet here, only the error. Otherwise, we would run into backoff
				LastFailedRevisionErrors: []string{"segfault", "falling back to last-known-good revision, might take up to 5 min"},
			},
			false,
			"operand pod failed: segfault",
			false,
		}},
		{"installer-succeeded-and-operand-fails-and-fallback-comes-up", []*corev1.Pod{
			installer("installer-4-test-node-1", corev1.PodSucceeded),
			fallbackOperand("test-pod", "2", corev1.PodRunning, true, timestamp("15:04:01"), "4", "CrashLooping", "pod is crash-looping"),
		}, 4, operatorv1.NodeStatus{
			NodeName:                 "test-node-1",
			CurrentRevision:          2,
			TargetRevision:           4,
			LastFailedRevisionErrors: []string{"pod is crash-looping", "will fall back to last-known-good revision"},
		}, true, Expected{
			&operatorv1.NodeStatus{
				NodeName:                 "test-node-1",
				CurrentRevision:          2,
				TargetRevision:           4,
				LastFailedCount:          0,
				LastFailedRevision:       4,
				LastFailedTime:           metav1TimestampPtr("15:04:01"),
				LastFailedRevisionErrors: []string{"fallback to last-known-good revision 2 took place after: pod is crash-looping (CrashLooping)"},
				LastFailedReason:         "OperandFailedFallback",
				LastFallbackCount:        1,
			},
			false,
			"static pod for 4 did not launch and last-known-good revision 2 has been started",
			false,
		}},
		{"after-retries-fallback-comes-up", []*corev1.Pod{
			installer("installer-4-retry-3-test-node-1", corev1.PodSucceeded),
			fallbackOperand("test-pod", "2", corev1.PodRunning, true, timestamp("15:04:01"), "4", "CrashLooping", "pod is crash-looping"),
		}, 4, operatorv1.NodeStatus{
			NodeName:                 "test-node-1",
			CurrentRevision:          2,
			TargetRevision:           4,
			LastFailedCount:          2,
			LastFailedRevision:       4,
			LastFailedTime:           metav1TimestampPtr("14:56:17"),
			LastFailedRevisionErrors: []string{"fallback to last-known-good revision 2 took place after: pod is crash-looping (CrashLooping)"},
			LastFailedReason:         "OperandFailedFallback",
			LastFallbackCount:        1,
		}, true, Expected{
			&operatorv1.NodeStatus{
				NodeName:                 "test-node-1",
				CurrentRevision:          2,
				TargetRevision:           4,
				LastFailedCount:          2,
				LastFailedRevision:       4,
				LastFailedTime:           metav1TimestampPtr("15:04:01"),
				LastFailedRevisionErrors: []string{"fallback to last-known-good revision 2 took place after: pod is crash-looping (CrashLooping)"},
				LastFailedReason:         "OperandFailedFallback",
				LastFallbackCount:        2,
			},
			false,
			"static pod for 4 did not launch and last-known-good revision 2 has been started",
			false,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			kubeClient := fake.NewSimpleClientset()
			for _, p := range test.pods {
				kubeClient.Tracker().Add(p)
			}
			kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test"))
			fakeStaticPodOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				&operatorv1.StaticPodOperatorStatus{
					LatestAvailableRevision: test.latestAvailableRevision,
					NodeStatuses:            []operatorv1.NodeStatus{test.current},
				},
				nil,
				nil,
			)
			eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{})
			c := NewInstallerController(
				"test", "test-pod",
				[]revision.RevisionResource{{Name: "test-config"}},
				[]revision.RevisionResource{{Name: "test-secret"}},
				[]string{"/bin/true", "--foo=test", "--bar"},
				kubeInformers,
				fakeStaticPodOperatorClient,
				kubeClient.CoreV1(),
				kubeClient.CoreV1(),
				kubeClient.CoreV1(),
				eventRecorder,
			)
			c.now = func() time.Time { return now.Time }
			c.startupMonitorEnabled = func() (bool, error) {
				return test.startupMonitorEnabled, nil
			}

			nodeStatus, installerPodFailed, reason, err := c.newNodeStateForInstallInProgress(context.TODO(), &test.current, test.latestAvailableRevision)
			if err == nil && test.expected.err {
				t.Fatal("expected error, but didn't get any")
			} else if err != nil && !test.expected.err {
				t.Fatalf("unexpected error: %v", err)
			}
			if nodeStatus == nil && test.expected.nodeStatus != nil {
				t.Errorf("expected nodeStatus, but got nil: %#v", test.expected.nodeStatus)
			} else if nodeStatus != nil && test.expected.nodeStatus == nil {
				t.Errorf("expected nil nodeStatus, but got: %#v", nodeStatus)
			} else if nodeStatus != nil && !reflect.DeepEqual(nodeStatus, test.expected.nodeStatus) {
				t.Errorf("unexpected nodeStatus, got:\n%#v\nexpected:\n%#v", *nodeStatus, *test.expected.nodeStatus)
			}
			if reason != test.expected.reason {
				t.Errorf("unexpected reason %q, expected %q", reason, test.expected.reason)
			}
			if installerPodFailed != test.expected.installerPodFailed {
				t.Errorf("unexpected installerPodFailed=%v, expected %v", installerPodFailed, test.expected.installerPodFailed)
			}
		})
	}
}

type testSyncInstallerBehaviour int

const (
	testSyncInstallerSuccess testSyncInstallerBehaviour = iota
	testSyncInstallerFails
	testSyncInstallerDisappears
)

type testSyncOperandBehaviour int

const (
	testSyncOperandReady testSyncOperandBehaviour = iota
	testSyncOperandFallback
)

func TestSync(t *testing.T) {
	type Test struct {
		name                  string
		installerBehaviour    testSyncInstallerBehaviour
		operandBehaviour      testSyncOperandBehaviour
		startupMonitorEnabled bool
	}
	for _, tst := range []Test{
		{"happy case", testSyncInstallerSuccess, testSyncOperandReady, false},
		{"installer fails", testSyncInstallerFails, testSyncOperandReady, false},
		{"installer disappears", testSyncInstallerDisappears, testSyncOperandReady, false},

		{"happy case with startup-monitor", testSyncInstallerSuccess, testSyncOperandReady, true},
		{"installer fails with startup-monitor", testSyncInstallerFails, testSyncOperandReady, true},
		{"installer disappears with startup-monitor", testSyncInstallerDisappears, testSyncOperandReady, true},
		{"fallback happens with startup-monitor", testSyncInstallerSuccess, testSyncOperandFallback, true},
	} {
		t.Run(tst.name, func(t *testing.T) {
			testSync(t, tst.installerBehaviour, tst.operandBehaviour, tst.startupMonitorEnabled)
		})
	}
}

func testSync(t *testing.T, firstInstallerBehaviour testSyncInstallerBehaviour, firstOperandBehaviour testSyncOperandBehaviour, startupMonitorEnabled bool) {
	kubeClient := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "test-config"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "test-secret"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: fmt.Sprintf("%s-%d", "test-secret", 1)}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: fmt.Sprintf("%s-%d", "test-config", 1)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: fmt.Sprintf("%s-%d", "test-secret", 3)}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: fmt.Sprintf("%s-%d", "test-config", 3)}},
	)

	var installerPods []*corev1.Pod

	kubeClient.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		for _, p := range installerPods {
			if p.Namespace == action.GetNamespace() && p.Name == pod.Name {
				return true, nil, errors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, pod.Name)
			}
		}
		installerPods = append(installerPods, pod)
		return true, pod, nil
	})
	kubeClient.PrependReactor("get", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		podName := action.(ktesting.GetAction).GetName()
		for _, p := range installerPods {
			if p.Namespace == action.GetNamespace() && p.Name == podName {
				return true, p, nil
			}
		}
		return false, nil, nil
	})

	kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test"))
	fakeStaticPodOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{
				ManagementState: operatorv1.Managed,
			},
		},
		&operatorv1.StaticPodOperatorStatus{
			LatestAvailableRevision: 3,
			NodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
					TargetRevision:  0,
				},
			},
		},
		nil,
		nil,
	)

	eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{})
	podCommand := []string{"/bin/true", "--foo=test", "--bar"}
	c := NewInstallerController(
		"test", "test-pod",
		[]revision.RevisionResource{{Name: "test-config"}},
		[]revision.RevisionResource{{Name: "test-secret"}},
		podCommand,
		kubeInformers,
		fakeStaticPodOperatorClient,
		kubeClient.CoreV1(),
		kubeClient.CoreV1(),
		kubeClient.CoreV1(),
		eventRecorder,
	)
	c.ownerRefsFn = func(ctx context.Context, revision int32) ([]metav1.OwnerReference, error) {
		return []metav1.OwnerReference{}, nil
	}
	c.installerPodImageFn = func() string { return "docker.io/foo/bar" }

	installerBackOffDuration, fallbackBackOffDuration := 0*time.Second, 0*time.Second
	c.installerBackOff = func(count int) time.Duration {
		return installerBackOffDuration
	}
	c.fallbackBackOff = func(count int) time.Duration {
		return fallbackBackOffDuration
	}
	withCustomInstallerBackOff := func(d time.Duration, fn func()) {
		old := installerBackOffDuration
		installerBackOffDuration = d
		defer func() { installerBackOffDuration = old }()
		fn()
	}
	c.startupMonitorEnabled = func() (bool, error) {
		return startupMonitorEnabled, nil
	}
	withCustomFallbackBackOff := func(d time.Duration, fn func()) {
		old := fallbackBackOffDuration
		fallbackBackOffDuration = d
		defer func() { fallbackBackOffDuration = old }()
		fn()
	}

	t.Log("setting target revision")
	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	if len(installerPods) > 0 {
		t.Fatalf("not expected to create installer pod yet")
	}

	_, currStatus, _, _ := fakeStaticPodOperatorClient.GetStaticPodOperatorState()
	if currStatus.NodeStatuses[0].TargetRevision != 3 {
		t.Fatalf("expected target revision 3, got: %d", currStatus.NodeStatuses[0].TargetRevision)
	}

	t.Log("starting installer pod")

	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}
	if len(installerPods) != 1 {
		t.Fatalf("expected to create one installer pod, got %d", len(installerPods))
	}

	cmd := installerPods[0].Spec.Containers[0].Command
	if !reflect.DeepEqual(podCommand, cmd) {
		t.Fatalf("expected pod command %#v to match resulting installer pod command: %#v", podCommand, cmd)
	}

	args := installerPods[0].Spec.Containers[0].Args
	if len(args) == 0 {
		t.Fatalf("pod args should not be empty")
	}
	foundRevision := false
	for _, arg := range args {
		if arg == "--revision=3" {
			foundRevision = true
		}
	}
	if !foundRevision {
		t.Fatalf("revision installer argument not found")
	}

	t.Log("syncing again, nothing happens")
	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	if currStatus.NodeStatuses[0].TargetRevision != 3 {
		t.Fatalf("expected target revision 3, got: %d", currStatus.NodeStatuses[0].TargetRevision)
	}
	if currStatus.NodeStatuses[0].CurrentRevision != 1 {
		t.Fatalf("expected current revision 1, got: %d", currStatus.NodeStatuses[0].CurrentRevision)
	}

	switch firstInstallerBehaviour {
	case testSyncInstallerSuccess:
		t.Log("installer succeeded")
		installerPods[0].Status.Phase = corev1.PodSucceeded
	case testSyncInstallerFails:
		t.Log("installer failed")

		installerPods[0].Status.Phase = corev1.PodFailed
		installerPods[0].Status.ContainerStatuses = []corev1.ContainerStatus{
			{
				Name: "installer",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Message: "fake death"},
				},
			},
		}

		if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
			t.Fatal(err)
		}

		_, currStatus, _, _ = fakeStaticPodOperatorClient.GetStaticPodOperatorState()
		if revision := currStatus.NodeStatuses[0].CurrentRevision; revision != 1 {
			t.Fatalf("expected current revision for node to be 1, got %d", revision)
		}
		if count := currStatus.NodeStatuses[0].LastFailedCount; count != 1 {
			t.Fatalf("expected failed count to be 1, got %d", count)
		}
		if c := v1helpers.FindOperatorCondition(currStatus.Conditions, condition.NodeInstallerProgressingConditionType); c == nil {
			t.Fatalf("missing %s condition", condition.NodeInstallerProgressingConditionType)
		} else if c.Status != operatorv1.ConditionTrue {
			t.Fatalf("expected %s condition to be true", condition.NodeInstallerProgressingConditionType)
		}
		if c := v1helpers.FindOperatorCondition(currStatus.Conditions, condition.NodeInstallerDegradedConditionType); c == nil {
			t.Fatalf("missing %s condition", condition.NodeInstallerDegradedConditionType)
		} else if c.Status != operatorv1.ConditionTrue {
			t.Fatalf("expected %s condition to be true", condition.NodeInstallerDegradedConditionType)
		}

		t.Log("try to launch 2nd installer with backoff, too early")

		withCustomInstallerBackOff(10*time.Second, func() {
			if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
				t.Fatal(err)
			}
		})
		if len(installerPods) != 1 {
			t.Fatal("didn't expect new installer pod yet due to backoff")
		}

		t.Log("launch 2nd installer when backoff has passed")

		withCustomInstallerBackOff(0*time.Second, func() {
			if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
				t.Fatal(err)
			}
		})
		if len(installerPods) != 2 {
			t.Fatal("expected second installer pod after failure")
		}
		if errs := currStatus.NodeStatuses[0].LastFailedRevisionErrors; len(errs) != 1 || !strings.Contains(errs[0], "fake death") {
			t.Fatalf("expected pod termination message in lastFailedRevisionErrors, but found: %s", errs)
		}
		if c := v1helpers.FindOperatorCondition(currStatus.Conditions, condition.NodeInstallerProgressingConditionType); c == nil {
			t.Fatalf("missing %s condition", condition.NodeInstallerProgressingConditionType)
		} else if c.Status != operatorv1.ConditionTrue {
			t.Fatalf("expected %s condition to be true", condition.NodeInstallerProgressingConditionType)
		}
		if c := v1helpers.FindOperatorCondition(currStatus.Conditions, condition.NodeInstallerDegradedConditionType); c == nil {
			t.Fatalf("missing %s condition", condition.NodeInstallerDegradedConditionType)
		} else if c.Status != operatorv1.ConditionTrue {
			t.Fatalf("expected %s condition to be true", condition.NodeInstallerDegradedConditionType)
		}

		t.Log("2nd installer succeeded")
		installerPods[1].Status.Phase = corev1.PodSucceeded

	case testSyncInstallerDisappears:
		t.Log("installer disappeared")
		installerPods = nil

		if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
			t.Fatal(err)
		}

		_, currStatus, _, _ = fakeStaticPodOperatorClient.GetStaticPodOperatorState()
		if revision := currStatus.NodeStatuses[0].CurrentRevision; revision != 1 {
			t.Fatalf("expected current revision for node to be 1, got %d", revision)
		}
		if count := currStatus.NodeStatuses[0].LastFailedCount; count != 0 {
			t.Fatalf("expected failed count to be 0, got %d", count)
		}
		if len(installerPods) != 1 {
			t.Fatal("expected second installer pod after first disappear")
		}
		if c := v1helpers.FindOperatorCondition(currStatus.Conditions, condition.NodeInstallerProgressingConditionType); c == nil {
			t.Fatalf("missing %s condition", condition.NodeInstallerProgressingConditionType)
		} else if c.Status != operatorv1.ConditionTrue {
			t.Fatalf("expected %s condition to be true", condition.NodeInstallerProgressingConditionType)
		}

		// NodeInstallerDegraded is not set because we don't see the prior existence of the installer in the state machine

		t.Log("2nd installer succeeded")
		installerPods[0].Status.Phase = corev1.PodSucceeded
	}

	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.GetStaticPodOperatorState()
	if revision := currStatus.NodeStatuses[0].CurrentRevision; revision != 1 {
		t.Errorf("expected current revision for node to be 1, got %d", revision)
	}

	t.Log("static pod launched, but is not ready")
	staticPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-pod-test-node-1",
			Namespace:         "test",
			Labels:            map[string]string{"revision": "3"},
			Annotations:       map[string]string{},
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Status: corev1.ConditionFalse,
					Type:   corev1.PodReady,
				},
			},
			Phase: corev1.PodRunning,
		},
	}
	kubeClient.PrependReactor("get", "pods", getPodsReactor(staticPod))

	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.GetStaticPodOperatorState()
	if revision := currStatus.NodeStatuses[0].CurrentRevision; revision != 1 {
		t.Fatalf("expected current revision for node to be 1, got %d", revision)
	}

	switch firstOperandBehaviour {
	case testSyncOperandReady:
		t.Log("static pod is ready")
		staticPod.Status.Conditions[0].Status = corev1.ConditionTrue

		if startupMonitorEnabled {
			t.Log("startup-monitor notices static pod and update nodeStatus")
			_, status, rv, err := fakeStaticPodOperatorClient.GetStaticPodOperatorState()
			if err != nil {
				t.Fatal(err)
			}
			status.NodeStatuses[0] = operatorv1.NodeStatus{
				NodeName:        "test-node-1",
				CurrentRevision: 3,
			}
			if _, err := fakeStaticPodOperatorClient.UpdateStaticPodOperatorStatus(context.TODO(), rv, status); err != nil {
				t.Fatal(err)
			}
		}

	case testSyncOperandFallback:
		t.Log("static pod is replaced with fallback by startup-monitor")

		staticFallbackPod := staticPod.DeepCopy()
		staticFallbackPod.Labels["revision"] = "1"
		staticFallbackPod.Annotations[annotations.FallbackForRevision] = "3"
		staticFallbackPod.Annotations[annotations.FallbackReason] = "ChashLooping"
		staticFallbackPod.Annotations[annotations.FallbackMessage] = "pod is crashlooping"
		staticFallbackPod.Status.Conditions[0].Status = corev1.ConditionTrue

		kubeClient.PrependReactor("get", "pods", getPodsReactor(staticFallbackPod))

		if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
			t.Fatal(err)
		}
		_, currStatus, _, _ = fakeStaticPodOperatorClient.GetStaticPodOperatorState()
		if currentRevision := currStatus.NodeStatuses[0].CurrentRevision; currentRevision != 1 {
			t.Fatalf("expected current revision for node to be 1, got %d", currentRevision)
		}
		if count := currStatus.NodeStatuses[0].LastFallbackCount; count != 1 {
			t.Fatalf("expected fail count for node to be 1, got %d", count)
		}

		t.Log("not yet expecting second installer due to 1 min fallback backoff")
		withCustomFallbackBackOff(10*time.Minute, func() {
			if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
				t.Fatal(err)
			}
			if len(installerPods) == 2 {
				t.Fatal("not yet expected 2nd installer pod immediately, only after 1s")
			}
		})

		t.Log("expecting second installer after waiting enough")
		if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
			t.Fatal(err)
		}
		if len(installerPods) != 2 {
			t.Fatal("expected 2nd installer pod")
		}

		t.Log("2nd installer succeeded")
		installerPods[1].Status.Phase = corev1.PodSucceeded

		if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
			t.Fatal(err)
		}

		_, currStatus, _, _ = fakeStaticPodOperatorClient.GetStaticPodOperatorState()
		if revision := currStatus.NodeStatuses[0].CurrentRevision; revision != 1 {
			t.Errorf("expected current revision for node to be 1, got %d", revision)
		}

		t.Log("new static pod launched")
		newStaticPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod-test-node-1",
				Namespace:   "test",
				Labels:      map[string]string{"revision": "3"},
				Annotations: map[string]string{},
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Status: corev1.ConditionTrue,
						Type:   corev1.PodReady,
					},
				},
				Phase: corev1.PodRunning,
			},
		}
		kubeClient.PrependReactor("get", "pods", getPodsReactor(newStaticPod))

		t.Log("startup-monitor notices 2nd static pod and update nodeStatus")
		_, status, rv, err := fakeStaticPodOperatorClient.GetStaticPodOperatorState()
		if err != nil {
			t.Fatal(err)
		}
		status.NodeStatuses[0] = operatorv1.NodeStatus{
			NodeName:        "test-node-1",
			CurrentRevision: 3,
		}
		if _, err := fakeStaticPodOperatorClient.UpdateStaticPodOperatorStatus(context.TODO(), rv, status); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.GetStaticPodOperatorState()
	if revision := currStatus.NodeStatuses[0].CurrentRevision; revision != 3 {
		t.Fatalf("expected current revision for node to be 3, got %d", revision)
	}
}

func getPodsReactor(pods ...*corev1.Pod) ktesting.ReactionFunc {
	return func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		podName := action.(ktesting.GetAction).GetName()
		for _, p := range pods {
			if p.Namespace == action.GetNamespace() && p.Name == podName {
				return true, p, nil
			}
		}
		return false, nil, nil
	}
}

func TestCreateInstallerPod(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "test-config"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "test-secret"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: fmt.Sprintf("%s-%d", "test-secret", 1)}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: fmt.Sprintf("%s-%d", "test-config", 1)}},
	)

	var installerPod *corev1.Pod
	kubeClient.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		installerPod = action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		return false, nil, nil
	})
	kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test"))

	fakeStaticPodOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{
				ManagementState: operatorv1.Managed,
			},
		},
		&operatorv1.StaticPodOperatorStatus{
			LatestAvailableRevision: 1,
			NodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-1",
					CurrentRevision: 0,
					TargetRevision:  0,
				},
			},
		},
		nil,
		nil,
	)
	eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{})

	c := NewInstallerController(
		"test", "test-pod",
		[]revision.RevisionResource{{Name: "test-config"}},
		[]revision.RevisionResource{{Name: "test-secret"}},
		[]string{"/bin/true"},
		kubeInformers,
		fakeStaticPodOperatorClient,
		kubeClient.CoreV1(),
		kubeClient.CoreV1(),
		kubeClient.CoreV1(),
		eventRecorder,
	)
	c.ownerRefsFn = func(ctx context.Context, revision int32) ([]metav1.OwnerReference, error) {
		return []metav1.OwnerReference{}, nil
	}
	c.installerPodImageFn = func() string { return "docker.io/foo/bar" }
	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	if installerPod != nil {
		t.Fatalf("expected first sync not to create installer pod")
	}

	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	if installerPod == nil {
		t.Fatalf("expected to create installer pod")
	}

	if installerPod.Spec.Containers[0].Image != "docker.io/foo/bar" {
		t.Fatalf("expected docker.io/foo/bar image, got %q", installerPod.Spec.Containers[0].Image)
	}

	if installerPod.Spec.Containers[0].Command[0] != "/bin/true" {
		t.Fatalf("expected /bin/true as a command, got %q", installerPod.Spec.Containers[0].Command[0])
	}

	if installerPod.Name != "installer-1-test-node-1" {
		t.Fatalf("expected name installer-1-test-node-1, got %q", installerPod.Name)
	}

	if installerPod.Namespace != "test" {
		t.Fatalf("expected test namespace, got %q", installerPod.Namespace)
	}

	expectedArgs := []string{
		"-v=2",
		"--revision=1",
		"--namespace=test",
		"--pod=test-config",
		"--resource-dir=/etc/kubernetes/static-pod-resources",
		"--pod-manifest-dir=/etc/kubernetes/manifests",
		"--configmaps=test-config",
		"--secrets=test-secret",
	}

	if len(expectedArgs) != len(installerPod.Spec.Containers[0].Args) {
		t.Fatalf("expected arguments does not match container arguments: %#v != %#v", expectedArgs, installerPod.Spec.Containers[0].Args)
	}

	for i, v := range installerPod.Spec.Containers[0].Args {
		if expectedArgs[i] != v {
			t.Errorf("arg[%d] expected %q, got %q", i, expectedArgs[i], v)
		}
	}
}

func TestEnsureInstallerPod(t *testing.T) {
	tests := []struct {
		name         string
		expectedArgs []string
		configs      []revision.RevisionResource
		secrets      []revision.RevisionResource
		expectedErr  string
	}{
		{
			name: "normal",
			expectedArgs: []string{
				"-v=2",
				"--revision=1",
				"--namespace=test",
				"--pod=test-config",
				"--resource-dir=/etc/kubernetes/static-pod-resources",
				"--pod-manifest-dir=/etc/kubernetes/manifests",
				"--configmaps=test-config",
				"--secrets=test-secret",
			},
			configs: []revision.RevisionResource{{Name: "test-config"}},
			secrets: []revision.RevisionResource{{Name: "test-secret"}},
		},
		{
			name: "optional",
			expectedArgs: []string{
				"-v=2",
				"--revision=1",
				"--namespace=test",
				"--pod=test-config",
				"--resource-dir=/etc/kubernetes/static-pod-resources",
				"--pod-manifest-dir=/etc/kubernetes/manifests",
				"--configmaps=test-config",
				"--configmaps=test-config-2",
				"--optional-configmaps=test-config-opt",
				"--secrets=test-secret",
				"--secrets=test-secret-2",
				"--optional-secrets=test-secret-opt",
			},
			configs: []revision.RevisionResource{
				{Name: "test-config"},
				{Name: "test-config-2"},
				{Name: "test-config-opt", Optional: true}},
			secrets: []revision.RevisionResource{
				{Name: "test-secret"},
				{Name: "test-secret-2"},
				{Name: "test-secret-opt", Optional: true}},
		},
		{
			name: "first-cm-not-optional",
			expectedArgs: []string{
				"-v=2",
				"--revision=1",
				"--namespace=test",
				"--pod=test-config",
				"--resource-dir=/etc/kubernetes/static-pod-resources",
				"--pod-manifest-dir=/etc/kubernetes/manifests",
				"--configmaps=test-config",
				"--secrets=test-secret",
			},
			configs:     []revision.RevisionResource{{Name: "test-config", Optional: true}},
			secrets:     []revision.RevisionResource{{Name: "test-secret"}},
			expectedErr: "pod configmap test-config is required, cannot be optional",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := fake.NewSimpleClientset()

			var installerPod *corev1.Pod
			kubeClient.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
				installerPod = action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
				return false, nil, nil
			})
			kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test"))

			fakeStaticPodOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				&operatorv1.StaticPodOperatorStatus{
					LatestAvailableRevision: 1,
					NodeStatuses: []operatorv1.NodeStatus{
						{
							NodeName:        "test-node-1",
							CurrentRevision: 0,
							TargetRevision:  0,
						},
					},
				},
				nil,
				nil,
			)
			eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{})

			c := NewInstallerController(
				"test", "test-pod",
				tt.configs,
				tt.secrets,
				[]string{"/bin/true"},
				kubeInformers,
				fakeStaticPodOperatorClient,
				kubeClient.CoreV1(),
				kubeClient.CoreV1(),
				kubeClient.CoreV1(),
				eventRecorder,
			)
			c.ownerRefsFn = func(ctx context.Context, revision int32) ([]metav1.OwnerReference, error) {
				return []metav1.OwnerReference{}, nil
			}
			err := c.ensureInstallerPod(context.TODO(), &operatorv1.StaticPodOperatorSpec{}, &operatorv1.NodeStatus{
				NodeName:       "test-node-1",
				TargetRevision: 1,
			})
			if err != nil {
				if tt.expectedErr == "" {
					t.Errorf("InstallerController.ensureInstallerPod() expected no error, got = %v", err)
					return
				}
				if tt.expectedErr != err.Error() {
					t.Errorf("InstallerController.ensureInstallerPod() got error = %v, wanted %s", err, tt.expectedErr)
					return
				}
				return
			}
			if tt.expectedErr != "" {
				t.Errorf("InstallerController.ensureInstallerPod() passed but expected error %s", tt.expectedErr)
			}

			if len(tt.expectedArgs) != len(installerPod.Spec.Containers[0].Args) {
				t.Fatalf("expected arguments does not match container arguments: %#v != %#v", tt.expectedArgs, installerPod.Spec.Containers[0].Args)
			}

			for i, v := range installerPod.Spec.Containers[0].Args {
				if tt.expectedArgs[i] != v {
					t.Errorf("arg[%d] expected %q, got %q", i, tt.expectedArgs[i], v)
				}
			}
		})
	}
}

func TestCreateInstallerPodMultiNode(t *testing.T) {
	tests := []struct {
		name                    string
		nodeStatuses            []operatorv1.NodeStatus
		staticPods              []*corev1.Pod
		latestAvailableRevision int32
		expectedUpgradeOrder    []int
		expectedRetryCounts     []int
		expectedSyncError       []bool
		updateStatusErrors      []error
		numOfInstallersOOM      int
		ownerRefsFn             func(ctx context.Context, revision int32) ([]metav1.OwnerReference, error)
	}{
		{
			name:                    "three fresh nodes",
			latestAvailableRevision: 1,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName: "test-node-0",
				},
				{
					NodeName: "test-node-1",
				},
				{
					NodeName: "test-node-2",
				},
			},
			expectedUpgradeOrder: []int{0, 1, 2},
		},
		{
			name:                    "three nodes with current revision, all static pods ready",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{0, 1, 2},
		},
		{
			name:                    "one node already transitioning",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
					TargetRevision:  2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
		},
		{
			name:                    "one node already transitioning, although it is newer",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 2,
					TargetRevision:  3,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
		},
		{
			name:                    "three nodes, 2 not updated, one with failure in last revision",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:           "test-node-1",
					CurrentRevision:    1,
					LastFailedRevision: 2,
					LastFailedCount:    1,
					TargetRevision:     2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
			expectedRetryCounts:  []int{1},
		},
		{
			name:                    "three nodes, 2 not updated, one with failure in last revision, with target and count missing",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:           "test-node-1",
					CurrentRevision:    1,
					LastFailedRevision: 2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
			expectedRetryCounts:  []int{},
		},
		{
			name:                    "three nodes, 2 not updated, one with failure in old revision",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 2,
				},
				{
					NodeName:           "test-node-1",
					CurrentRevision:    2,
					LastFailedRevision: 1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 2,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 2, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{0, 1, 2},
		},
		{
			name:                    "three nodes with outdated current revision, second static pods unready",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, false),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
		},
		{
			name:                    "three nodes with outdated current revision and new target revision set, but failed",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:           "test-node-1",
					CurrentRevision:    1,
					LastFailedRevision: 2,
					TargetRevision:     2,
					LastFailedCount:    10,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
			expectedRetryCounts:  []int{10, 0, 0},
		},
		{
			name:                    "three nodes with outdated current revision, installer failed, no target revision set",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:           "test-node-1",
					CurrentRevision:    1,
					LastFailedRevision: 2,
					TargetRevision:     2,
					LastFailedCount:    1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
			expectedRetryCounts:  []int{1, 0, 0},
		},
		{
			name:                    "three nodes with outdated current revision, installer failed, no target revision set, with target and count missing",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:           "test-node-1",
					CurrentRevision:    1,
					LastFailedRevision: 2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
			expectedRetryCounts:  []int{0, 0, 0},
		},
		{
			name:                    "three nodes with outdated current revision, installer failed, no target revision set, but new operand launched and is ready",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:           "test-node-1",
					CurrentRevision:    1,
					TargetRevision:     2,
					LastFailedRevision: 2,
					LastFailedCount:    10,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
			expectedRetryCounts:  []int{10, 0, 0},
		},
		{
			name:                    "three nodes with outdated current revision, installer failed, no target revision set, but new operand launched and is not ready",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:           "test-node-1",
					CurrentRevision:    1,
					TargetRevision:     2,
					LastFailedRevision: 2,
					LastFailedCount:    10,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 2, corev1.PodRunning, false),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
			expectedRetryCounts:  []int{10, 0, 0},
		},
		{
			name:                    "four nodes with outdated current revision, installer of 2nd was OOM killed, two more OOM happen, then success",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 2,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			// we call sync 2*3 times:
			// 1. notice update of node 0
			// 2. create installer for node 0, OOM, fall-through, notice update of node 0
			// 3. create installer for node 0, retry 0, OOM, fall-through, notice update of node 0
			// 4. create installer for node 0, retry 1, which succeeds, set CurrentRevision
			// 5. notice update of node 1
			// 6. create installer for node 1, which succeeds, set CurrentRevision
			expectedUpgradeOrder: []int{1, 1, 1, 2},
			expectedRetryCounts:  []int{0, 1, 2, 0},
			numOfInstallersOOM:   2,
		},
		{
			name:                    "three nodes with outdated current revision, 2nd & 3rd static pods unready",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, false),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, false),
			},
			expectedUpgradeOrder: []int{1, 2, 0},
		},
		{
			name:                    "updated node unready and newer version available, but updated again before older nodes are touched",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 2, corev1.PodRunning, false),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
		},
		{
			name:                    "two nodes on revision 1 and one node on revision 4",
			latestAvailableRevision: 5,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 4,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 4, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 2, 0},
		},
		{
			name:                    "two nodes 2 revisions behind and 1 node on latest available revision",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 3,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 3, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodSucceeded, true),
			},
			expectedUpgradeOrder: []int{1, 2},
		},
		{
			name:                    "two nodes at different revisions behind and 1 node on latest available revision",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 3,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 3, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodSucceeded, true),
			},
			expectedUpgradeOrder: []int{2, 1},
		},
		{
			name:                    "second node with old static pod than current revision",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 2,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 2,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, false),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 2, corev1.PodRunning, false),
			},
			expectedUpgradeOrder: []int{1, 2, 0},
		},
		{
			name:                    "first update status fails",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName: "test-node-1",
				},
			},
			expectedUpgradeOrder: []int{0},
			updateStatusErrors:   []error{errors.NewInternalError(fmt.Errorf("unknown"))},
			expectedSyncError:    []bool{true},
		},
		{
			name:                    "three nodes, 2 not updated, one already transitioning to a revision which is no longer available",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
					TargetRevision:  2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
			ownerRefsFn: func(ctx context.Context, revision int32) (references []metav1.OwnerReference, err error) {
				if revision == 3 {
					return []metav1.OwnerReference{}, nil
				}
				return nil, fmt.Errorf("TEST")
			},
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			createdInstallerPods := []*corev1.Pod{}
			installerPods := map[string]*corev1.Pod{}
			updatedStaticPods := map[string]*corev1.Pod{}

			namespace := fmt.Sprintf("test-%d", i)

			installerNodeAndID := func(installerName string) (string, int, int) {
				pat := `^installer-([0-9]*)-(retry-([0-9]*)-)?(.*)$`
				re := regexp.MustCompile(pat)
				ss := re.FindStringSubmatch(installerName)
				if len(ss) < 5 {
					t.Fatalf("unexpected isntaller pod name %q, expected to match %q: %v", installerName, pat, ss)
				}
				revision, err := strconv.Atoi(ss[1])
				if err != nil {
					t.Fatalf("unexpected revision derived from installer pod name %q: %v", installerName, err)
				}
				var retry int
				if len(ss[2]) > 0 {
					retry, err = strconv.Atoi(ss[3])
					if err != nil {
						t.Fatalf("unexpected retry count derived from installer pod name %q: %v", installerName, err)
					}
				}
				return ss[4], revision, retry
			}

			kubeClient := fake.NewSimpleClientset(
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-secret"}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-config"}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: fmt.Sprintf("%s-%d", "test-secret", test.latestAvailableRevision)}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: fmt.Sprintf("%s-%d", "test-config", test.latestAvailableRevision)}},
			)
			kubeClient.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
				createdPod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
				createdInstallerPods = append(createdInstallerPods, createdPod)
				if _, found := installerPods[createdPod.Name]; found {
					return false, nil, errors.NewAlreadyExists(corev1.SchemeGroupVersion.WithResource("pods").GroupResource(), createdPod.Name)
				}
				installerPods[createdPod.Name] = createdPod
				if test.numOfInstallersOOM > 0 {
					test.numOfInstallersOOM--

					createdPod.Status.Phase = corev1.PodFailed
					createdPod.Status.ContainerStatuses = []corev1.ContainerStatus{
						{
							Name: "container",
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									ExitCode: 1,
									Reason:   "OOMKilled",
									Message:  "killed by OOM",
								},
							},
							Ready: false,
						},
					}
				} else {
					// Once the installer pod is created, set its status to succeeded.
					// Note that in reality, this will probably take couple sync cycles to happen, however it is useful to do this fast
					// to rule out timing bugs.
					createdPod.Status.Phase = corev1.PodSucceeded

					nodeName, id, _ := installerNodeAndID(createdPod.Name)
					staticPodName := mirrorPodNameForNode("test-pod", nodeName)

					updatedStaticPods[staticPodName] = newStaticPod(staticPodName, id, corev1.PodRunning, true)
				}

				return true, nil, nil
			})

			// When newNodeStateForInstallInProgress ask for pod, give it a pod that already succeeded.
			kubeClient.PrependReactor("get", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
				podName := action.(ktesting.GetAction).GetName()
				if pod, found := installerPods[podName]; found {
					return true, pod, nil
				}
				if pod, exists := updatedStaticPods[podName]; exists {
					if pod == nil {
						return false, nil, nil
					}
					return true, pod, nil
				}
				for _, pod := range test.staticPods {
					if pod.Name == podName {
						return true, pod, nil
					}
				}
				return false, nil, nil
			})
			kubeClient.PrependReactor("delete", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
				podName := action.(ktesting.GetAction).GetName()
				if pod, found := installerPods[podName]; found {
					delete(installerPods, podName)
					return true, pod, nil
				}
				return false, nil, nil
			})

			kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test-"+test.name))
			statusUpdateCount := 0
			statusUpdateErrorFunc := func(rv string, status *operatorv1.StaticPodOperatorStatus) error {
				var err error
				if statusUpdateCount < len(test.updateStatusErrors) {
					err = test.updateStatusErrors[statusUpdateCount]
				}
				statusUpdateCount++
				return err
			}
			fakeStaticPodOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				&operatorv1.StaticPodOperatorStatus{
					LatestAvailableRevision: test.latestAvailableRevision,
					NodeStatuses:            test.nodeStatuses,
				},
				statusUpdateErrorFunc,
				nil,
			)

			eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{})

			c := NewInstallerController(
				namespace, "test-pod",
				[]revision.RevisionResource{{Name: "test-config"}},
				[]revision.RevisionResource{{Name: "test-secret"}},
				[]string{"/bin/true"},
				kubeInformers,
				fakeStaticPodOperatorClient,
				kubeClient.CoreV1(),
				kubeClient.CoreV1(),
				kubeClient.CoreV1(),
				eventRecorder,
			)
			c.ownerRefsFn = func(ctx context.Context, revision int32) ([]metav1.OwnerReference, error) {
				return []metav1.OwnerReference{}, nil
			}
			if test.ownerRefsFn != nil {
				c.ownerRefsFn = test.ownerRefsFn
			}
			c.installerPodImageFn = func() string { return "docker.io/foo/bar" }
			c.installerBackOff = func(count int) time.Duration {
				return 0 // switch off for this test
			}

			// Each node needs at least 2 syncs to first create the pod and then acknowledge its existence.
			// We switch off backoff which would lead to more sync.
			for i := 1; i <= len(test.nodeStatuses)*2+1; i++ {
				err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder))
				expectedErr := false
				if i-1 < len(test.expectedSyncError) && test.expectedSyncError[i-1] {
					expectedErr = true
				}
				if err != nil && !expectedErr {
					t.Errorf("failed to execute %d sync: %v", i, err)
				} else if err == nil && expectedErr {
					t.Errorf("expected sync error in sync %d, but got nil", i)
				}
			}

			for i := range test.expectedUpgradeOrder {
				if i >= len(createdInstallerPods) {
					t.Fatalf("expected more (got only %d) installer pods in the node order %v", len(createdInstallerPods), test.expectedUpgradeOrder[i:])
				}

				nodeName, revision, retry := installerNodeAndID(createdInstallerPods[i].Name)
				if expected, got := test.nodeStatuses[test.expectedUpgradeOrder[i]].NodeName, nodeName; expected != got {
					t.Errorf("expected installer pod number %d to be for node %q, but got %q", i, expected, got)
				}

				expectedRetry := 0
				if i < len(test.expectedRetryCounts) {
					expectedRetry = test.expectedRetryCounts[i]
				}
				if expected, got := revision, int(test.latestAvailableRevision); expected != got {
					t.Errorf("expected revision %d in round %d, but got %d", expected, i, got)
				} else if expectedRetry != retry {
					t.Errorf("expected retry count %d in round %d, but got %d", expectedRetry, i, retry)
				}
			}
			if len(test.expectedUpgradeOrder) < len(createdInstallerPods) {
				t.Errorf("too many installer pods created, expected %d, got %d", len(test.expectedUpgradeOrder), len(createdInstallerPods))
			}
		})
	}

}

func TestInstallerController_manageInstallationPods(t *testing.T) {
	type fields struct {
		targetNamespace      string
		staticPodName        string
		configMaps           []revision.RevisionResource
		secrets              []revision.RevisionResource
		command              []string
		operatorConfigClient v1helpers.StaticPodOperatorClient
		kubeClient           kubernetes.Interface
		eventRecorder        events.Recorder
		installerPodImageFn  func() string
	}
	type args struct {
		operatorSpec           *operatorv1.StaticPodOperatorSpec
		originalOperatorStatus *operatorv1.StaticPodOperatorStatus
		resourceVersion        string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    bool
		wantErr bool
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &InstallerController{
				targetNamespace:     tt.fields.targetNamespace,
				staticPodName:       tt.fields.staticPodName,
				configMaps:          tt.fields.configMaps,
				secrets:             tt.fields.secrets,
				command:             tt.fields.command,
				operatorClient:      tt.fields.operatorConfigClient,
				configMapsGetter:    tt.fields.kubeClient.CoreV1(),
				podsGetter:          tt.fields.kubeClient.CoreV1(),
				eventRecorder:       tt.fields.eventRecorder,
				installerPodImageFn: tt.fields.installerPodImageFn,
			}
			got, _, err := c.manageInstallationPods(context.TODO(), tt.args.operatorSpec, tt.args.originalOperatorStatus)
			if (err != nil) != tt.wantErr {
				t.Errorf("InstallerController.manageInstallationPods() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("InstallerController.manageInstallationPods() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNodeToStartRevisionWith(t *testing.T) {
	type StaticPod struct {
		name     string
		state    staticPodState
		revision int32
	}
	type Test struct {
		name        string
		nodes       []operatorv1.NodeStatus
		pods        []StaticPod
		expected    int
		expectedErr bool
	}

	newNode := func(name string, current, target int32) operatorv1.NodeStatus {
		return operatorv1.NodeStatus{NodeName: name, CurrentRevision: current, TargetRevision: target}
	}

	for _, test := range []Test{
		{
			name:        "empty",
			expectedErr: true,
		},
		{
			name: "no pods",
			pods: nil,
			nodes: []operatorv1.NodeStatus{
				newNode("a", 0, 0),
				newNode("b", 0, 0),
				newNode("c", 0, 0),
			},
			expected: 0,
		},
		{
			name: "all ready",
			pods: []StaticPod{
				{"a", staticPodStateReady, 1},
				{"b", staticPodStateReady, 1},
				{"c", staticPodStateReady, 1},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 1, 0),
				newNode("b", 1, 0),
				newNode("c", 1, 0),
			},
			expected: 0,
		},
		{
			name: "one failed",
			pods: []StaticPod{
				{"a", staticPodStateReady, 1},
				{"b", staticPodStateReady, 1},
				{"c", staticPodStateFailed, 1},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 1, 0),
				newNode("b", 1, 0),
				newNode("c", 1, 0),
			},
			expected: 2,
		},
		{
			name: "one pending",
			pods: []StaticPod{
				{"a", staticPodStateReady, 1},
				{"b", staticPodStateReady, 1},
				{"c", staticPodStatePending, 1},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 1, 0),
				newNode("b", 1, 0),
				newNode("c", 0, 0),
			},
			expected: 2,
		},
		{
			name: "multiple pending",
			pods: []StaticPod{
				{"a", staticPodStateReady, 1},
				{"b", staticPodStatePending, 1},
				{"c", staticPodStatePending, 1},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 1, 0),
				newNode("b", 0, 0),
				newNode("c", 0, 0),
			},
			expected: 1,
		},
		{
			name: "one updating",
			pods: []StaticPod{
				{"a", staticPodStateReady, 1},
				{"b", staticPodStatePending, 0},
				{"c", staticPodStateReady, 0},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 1, 0),
				newNode("b", 0, 1),
				newNode("c", 0, 0),
			},
			expected: 1,
		},
		{
			name: "pods missing",
			pods: []StaticPod{
				{"a", staticPodStateReady, 1},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 1, 0),
				newNode("b", 0, 0),
				newNode("c", 0, 0),
			},
			expected: 1,
		},
		{
			name: "one old",
			pods: []StaticPod{
				{"a", staticPodStateReady, 2},
				{"b", staticPodStateReady, 1},
				{"c", staticPodStateReady, 2},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 2, 0),
				newNode("b", 2, 0),
				newNode("c", 2, 0),
			},
			expected: 1,
		},
		{
			name: "one behind, but as stated",
			pods: []StaticPod{
				{"a", staticPodStateReady, 2},
				{"b", staticPodStateReady, 1},
				{"c", staticPodStateReady, 2},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 2, 0),
				newNode("b", 1, 0),
				newNode("c", 2, 0),
			},
			expected: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakeGetStaticPodState := func(ctx context.Context, nodeName string) (state staticPodState, revision, reason string, errs []string, ts time.Time, err error) {
				for _, p := range test.pods {
					if p.name == nodeName {
						return p.state, strconv.Itoa(int(p.revision)), "", nil, time.Now(), nil
					}
				}
				return staticPodStatePending, "", "", nil, time.Now(), errors.NewNotFound(schema.GroupResource{Resource: "pods"}, nodeName)
			}
			i, _, err := nodeToStartRevisionWith(context.TODO(), fakeGetStaticPodState, test.nodes)
			if err == nil && test.expectedErr {
				t.Fatalf("expected error, got none")
			}
			if err != nil && !test.expectedErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if i != test.expected {
				t.Errorf("expected node ID %d, got %d", test.expected, i)
			}
		})
	}
}

func TestSetConditions(t *testing.T) {
	type TestCase struct {
		name                      string
		latestAvailableRevision   int32
		lastFailedRevision        int32
		lastFailedTime            *metav1.Time
		lastFailedCount           int
		targetRevision            int32
		currentRevisions          []int32
		expectedAvailableStatus   operatorv1.ConditionStatus
		expectedProgressingStatus operatorv1.ConditionStatus
		expectedFailingStatus     operatorv1.ConditionStatus
	}

	testCase := func(name string, available, progressing, failed bool, lastFailedRevision int32, lastFailedCount int, latest, target int32, current ...int32) TestCase {
		availableStatus := operatorv1.ConditionFalse
		pendingStatus := operatorv1.ConditionFalse
		expectedFailingStatus := operatorv1.ConditionFalse
		if available {
			availableStatus = operatorv1.ConditionTrue
		}
		if progressing {
			pendingStatus = operatorv1.ConditionTrue
		}
		if failed {
			expectedFailingStatus = operatorv1.ConditionTrue
		}
		var lastFailedTime *metav1.Time
		if lastFailedRevision != 0 {
			now := metav1.NewTime(time.Now())
			lastFailedTime = &now
		}
		return TestCase{name, latest, lastFailedRevision, lastFailedTime, lastFailedCount, target, current, availableStatus, pendingStatus, expectedFailingStatus}
	}

	testCases := []TestCase{
		testCase("AvailableProgressingDegraded", true, true, true, 3, 1, 2, 3, 2, 1, 2, 1),
		testCase("AvailableProgressingDegraded", true, true, true, 3, 3, 2, 3, 2, 1, 2, 1),
		testCase("AvailableProgressing", true, true, false, 1, 3, 2, 2, 2, 1, 2, 1),
		testCase("AvailableProgressing", true, true, false, 0, 1, 2, 2, 2, 1, 2, 1),
		testCase("AvailableNotProgressing", true, false, false, 0, 1, 2, 2, 2, 2, 2),
		testCase("NotAvailableProgressing", false, true, false, 0, 1, 2, 2, 0, 0),
		testCase("NotAvailableAtOldLevelProgressing", true, true, false, 0, 1, 2, 2, 1, 1),
		testCase("NotAvailableNotProgressing", false, false, false, 0, 1, 2, 2),
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			status := &operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: tc.latestAvailableRevision,
			}
			for _, current := range tc.currentRevisions {
				status.NodeStatuses = append(status.NodeStatuses, operatorv1.NodeStatus{
					CurrentRevision:    current,
					LastFailedRevision: tc.lastFailedRevision,
					LastFailedTime:     tc.lastFailedTime,
					LastFailedCount:    tc.lastFailedCount,
					TargetRevision:     tc.targetRevision,
				})
			}
			setAvailableProgressingNodeInstallerFailingConditions(status)

			availableCondition := v1helpers.FindOperatorCondition(status.Conditions, condition.StaticPodsAvailableConditionType)
			if availableCondition == nil {
				t.Error("Available condition: not found")
			} else if availableCondition.Status != tc.expectedAvailableStatus {
				t.Errorf("Available condition: expected status %v, actual status %v", tc.expectedAvailableStatus, availableCondition.Status)
			}

			pendingCondition := v1helpers.FindOperatorCondition(status.Conditions, condition.NodeInstallerProgressingConditionType)
			if pendingCondition == nil {
				t.Error("Progressing condition: not found")
			} else if pendingCondition.Status != tc.expectedProgressingStatus {
				t.Errorf("Progressing condition: expected status %v, actual status %v", tc.expectedProgressingStatus, pendingCondition.Status)
			}

			failingCondition := v1helpers.FindOperatorCondition(status.Conditions, condition.NodeInstallerDegradedConditionType)
			if failingCondition == nil {
				t.Error("Failing condition: not found")
			} else if failingCondition.Status != tc.expectedFailingStatus {
				t.Errorf("Failing condition: expected status %v, actual status %v", tc.expectedFailingStatus, failingCondition.Status)
			}
		})
	}

}

func TestEnsureRequiredResources(t *testing.T) {
	tests := []struct {
		name           string
		certConfigMaps []UnrevisionedResource
		certSecrets    []UnrevisionedResource

		revisionNumber int32
		configMaps     []revision.RevisionResource
		secrets        []revision.RevisionResource

		startingResources []runtime.Object
		expectedErr       string
	}{
		{
			name: "none",
		},
		{
			name: "skip-optional",
			certConfigMaps: []UnrevisionedResource{
				{Name: "foo-cm", Optional: true},
			},
			certSecrets: []UnrevisionedResource{
				{Name: "foo-s", Optional: true},
			},
		},
		{
			name: "wait-required",
			configMaps: []revision.RevisionResource{
				{Name: "foo-cm"},
			},
			secrets: []revision.RevisionResource{
				{Name: "foo-s"},
			},
			expectedErr: "missing required resources: [configmaps: foo-cm-0, secrets: foo-s-0]",
		},
		{
			name: "found-required",
			configMaps: []revision.RevisionResource{
				{Name: "foo-cm"},
			},
			secrets: []revision.RevisionResource{
				{Name: "foo-s"},
			},
			startingResources: []runtime.Object{
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "foo-cm-0"}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "foo-s-0"}},
			},
		},
		{
			name: "wait-required-certs",
			certConfigMaps: []UnrevisionedResource{
				{Name: "foo-cm"},
			},
			certSecrets: []UnrevisionedResource{
				{Name: "foo-s"},
			},
			expectedErr: "missing required resources: [configmaps: foo-cm, secrets: foo-s]",
		},
		{
			name: "found-required-certs",
			certConfigMaps: []UnrevisionedResource{
				{Name: "foo-cm"},
			},
			certSecrets: []UnrevisionedResource{
				{Name: "foo-s"},
			},
			startingResources: []runtime.Object{
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "foo-cm"}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "foo-s"}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(test.startingResources...)
			c := &InstallerController{
				targetNamespace: "ns",
				certConfigMaps:  test.certConfigMaps,
				certSecrets:     test.certSecrets,
				configMaps:      test.configMaps,
				secrets:         test.secrets,
				eventRecorder:   eventstesting.NewTestingEventRecorder(t),

				configMapsGetter: client.CoreV1(),
				secretsGetter:    client.CoreV1(),
			}

			actual := c.ensureRequiredResourcesExist(context.TODO(), test.revisionNumber)
			switch {
			case len(test.expectedErr) == 0 && actual == nil:
			case len(test.expectedErr) == 0 && actual != nil:
				t.Fatal(actual)
			case len(test.expectedErr) != 0 && actual == nil:
				t.Fatal(actual)
			case len(test.expectedErr) != 0 && actual != nil && !strings.Contains(actual.Error(), test.expectedErr):
				t.Fatalf("actual error: %q does not match expected: %q", actual.Error(), test.expectedErr)
			}

		})
	}
}

func newStaticPod(name string, revision int, phase corev1.PodPhase, ready bool) *corev1.Pod {
	condStatus := corev1.ConditionTrue
	if !ready {
		condStatus = corev1.ConditionFalse
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test",
			Labels:    map[string]string{"revision": strconv.Itoa(revision)},
		},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Status: condStatus,
					Type:   corev1.PodReady,
				},
			},
			Phase: phase,
		},
	}
}

func newStaticPodWithReadyTime(name string, revision int, phase corev1.PodPhase, ready bool, readyTime time.Time) *corev1.Pod {
	ret := newStaticPod(name, revision, phase, ready)
	ret.Status.Conditions[0].LastTransitionTime = metav1.NewTime(readyTime)
	return ret
}

func TestTimeToWait(t *testing.T) {
	nodeStatuses := []operatorv1.NodeStatus{
		{
			NodeName: "test-node-1",
		},
		{
			NodeName: "test-node-2",
		},
		{
			NodeName: "test-node-3",
		},
	}
	fakeNow := time.Now()
	fakeClock := clocktesting.NewFakeClock(fakeNow)

	tenSecondsAgo := fakeNow.Add(-10 * time.Second)
	thirtySecondsAgo := fakeNow.Add(-30 * time.Second)
	thirtyMinutesAgo := fakeNow.Add(-30 * time.Minute)

	tests := []struct {
		name            string
		minReadySeconds time.Duration
		staticPods      []runtime.Object
		expected        time.Duration
	}{
		{
			name:            "all long ready",
			minReadySeconds: 35 * time.Second,
			staticPods: []runtime.Object{
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true, thirtyMinutesAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true, thirtyMinutesAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true, thirtyMinutesAgo),
			},
			expected: 0,
		},
		{
			name:            "no pods",
			minReadySeconds: 35 * time.Second,
			expected:        0 * time.Second,
		},
		{
			name:            "exact match",
			minReadySeconds: 30 * time.Second,
			staticPods: []runtime.Object{
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true, thirtySecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true, thirtyMinutesAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true, thirtyMinutesAgo),
			},
			expected: 0,
		},
		{
			name:            "one short",
			minReadySeconds: 30 * time.Second,
			staticPods: []runtime.Object{
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true, thirtySecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, false, tenSecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true, tenSecondsAgo),
			},
			expected: 20 * time.Second,
		},
		{
			name:            "one not ready",
			minReadySeconds: 30 * time.Second,
			staticPods: []runtime.Object{
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true, thirtySecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, false, tenSecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true, tenSecondsAgo),
			},
			expected: 20 * time.Second,
		},
		{
			name:            "none ready",
			minReadySeconds: 30 * time.Second,
			staticPods: []runtime.Object{
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, false, thirtySecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, false, tenSecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, false, tenSecondsAgo),
			},
			expected: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kubeClient := fake.NewSimpleClientset(test.staticPods...)

			c := &InstallerController{
				targetNamespace:  "test",
				staticPodName:    "test-pod",
				minReadyDuration: test.minReadySeconds,
				podsGetter:       kubeClient.CoreV1(),
				clock:            fakeClock,
			}

			actual := c.timeToWaitBeforeInstallingNextPod(context.TODO(), nodeStatuses)
			if actual != test.expected {
				t.Fatal(actual)
			}
		})
	}
}

func timestamp(s string) time.Time {
	t, err := time.Parse(time.RFC3339, fmt.Sprintf("2021-01-02T%sZ", s))
	if err != nil {
		panic(err)
	}
	return t
}

func metav1Timestamp(s string) metav1.Time {
	return metav1.NewTime(timestamp(s))
}

func metav1TimestampPtr(s string) *metav1.Time {
	t := metav1Timestamp(s)
	return &t
}
