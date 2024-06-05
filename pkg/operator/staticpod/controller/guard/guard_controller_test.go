package guard

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	configv1 "github.com/openshift/api/config/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configlistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	staticcontrollercommon "github.com/openshift/cluster-etcd-operator/pkg/operator/staticpod/controller/common"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
)

type FakeInfrastructureInformer struct {
	Informer_ cache.SharedIndexInformer
	Lister_   configlistersv1.InfrastructureLister
}

func (f FakeInfrastructureInformer) Informer() cache.SharedIndexInformer {
	return f.Informer_
}

func (f FakeInfrastructureInformer) Lister() configlistersv1.InfrastructureLister {
	return f.Lister_
}

var _ configv1informers.InfrastructureInformer = &FakeInfrastructureInformer{}

type FakeInfrastructureLister struct {
	InfrastructureLister_ configlistersv1.InfrastructureLister
}

func (l FakeInfrastructureLister) Get(name string) (*configv1.Infrastructure, error) {
	return l.InfrastructureLister_.Get(name)
}

func (l FakeInfrastructureLister) List(selector labels.Selector) (ret []*configv1.Infrastructure, err error) {
	return l.InfrastructureLister_.List(selector)
}

type FakeInfrastructureSharedInformer struct {
	HasSynced_ bool
}

func (i FakeInfrastructureSharedInformer) RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error {
	//TODO implement me
	panic("implement me")
}

func (i FakeInfrastructureSharedInformer) IsStopped() bool {
	//TODO implement me
	panic("implement me")
}

func (i FakeInfrastructureSharedInformer) AddIndexers(indexers cache.Indexers) error { return nil }
func (i FakeInfrastructureSharedInformer) GetIndexer() cache.Indexer                 { return nil }
func (i FakeInfrastructureSharedInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (i FakeInfrastructureSharedInformer) AddEventHandlerWithResyncPeriod(handler cache.ResourceEventHandler, resyncPeriod time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (i FakeInfrastructureSharedInformer) GetStore() cache.Store           { return nil }
func (i FakeInfrastructureSharedInformer) GetController() cache.Controller { return nil }
func (i FakeInfrastructureSharedInformer) Run(stopCh <-chan struct{})      {}
func (i FakeInfrastructureSharedInformer) HasSynced() bool                 { return i.HasSynced_ }
func (i FakeInfrastructureSharedInformer) LastSyncResourceVersion() string { return "" }
func (i FakeInfrastructureSharedInformer) SetWatchErrorHandler(handler cache.WatchErrorHandler) error {
	return nil
}
func (i FakeInfrastructureSharedInformer) SetTransform(f cache.TransformFunc) error {
	return nil
}

func TestIsSNOCheckFnc(t *testing.T) {
	tests := []struct {
		name                      string
		infraObject               *configv1.Infrastructure
		hasSynced                 bool
		result, precheckSucceeded bool
		err                       bool
	}{
		{
			name: "Infrastructure informer has not synced",
			infraObject: &configv1.Infrastructure{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.InfrastructureStatus{
					ControlPlaneTopology: configv1.SingleReplicaTopologyMode,
				},
			},
			hasSynced:         false,
			precheckSucceeded: false,
		},
		{
			name: "Missing Infrastructure status",
			infraObject: &configv1.Infrastructure{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.InfrastructureStatus{},
			},
			hasSynced:         true,
			err:               true,
			precheckSucceeded: true,
		},
		{
			name: "Missing ControlPlaneTopology",
			infraObject: &configv1.Infrastructure{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.InfrastructureStatus{
					ControlPlaneTopology: "",
				},
			},
			hasSynced:         true,
			err:               true,
			precheckSucceeded: true,
		},
		{
			name: "ControlPlaneTopology not SingleReplicaTopologyMode",
			infraObject: &configv1.Infrastructure{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.InfrastructureStatus{
					ControlPlaneTopology: configv1.HighlyAvailableTopologyMode,
				},
			},
			hasSynced:         true,
			result:            false,
			precheckSucceeded: true,
		},
		{
			name: "ControlPlaneTopology is SingleReplicaTopologyMode",
			infraObject: &configv1.Infrastructure{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.InfrastructureStatus{
					ControlPlaneTopology: configv1.SingleReplicaTopologyMode,
				},
			},
			hasSynced:         true,
			result:            true,
			precheckSucceeded: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			if err := indexer.Add(test.infraObject); err != nil {
				t.Fatal(err.Error())
			}

			informer := FakeInfrastructureInformer{
				Informer_: FakeInfrastructureSharedInformer{
					HasSynced_: test.hasSynced,
				},
				Lister_: FakeInfrastructureLister{
					InfrastructureLister_: configlistersv1.NewInfrastructureLister(indexer),
				},
			}

			conditionalFunction := staticcontrollercommon.NewIsSingleNodePlatformFn(informer)
			result, precheckSucceeded, err := conditionalFunction()
			if test.err {
				if err == nil {
					t.Errorf("%s: expected error, got none", test.name)
				}
			} else {
				if err != nil {
					t.Errorf("%s: unexpected error: %v", test.name, err)
				} else if result != test.result || precheckSucceeded != test.precheckSucceeded {
					t.Errorf("%s: expected result %v, got %v, expected precheckSucceeded %v, got %v", test.name, test.result, result, test.precheckSucceeded, precheckSucceeded)
				}
			}
		})
	}
}

func fakeMasterNode(name string) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"node-role.kubernetes.io/master": "",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	return n
}

type FakeSyncContext struct {
	recorder events.Recorder
}

func (f FakeSyncContext) Queue() workqueue.RateLimitingInterface {
	return nil
}

func (f FakeSyncContext) QueueKey() string {
	return ""
}

func (f FakeSyncContext) Recorder() events.Recorder {
	return f.recorder
}

// render a guarding pod
func TestRenderGuardPod(t *testing.T) {
	unschedulableMasterNode := fakeMasterNode("master1")
	unschedulableMasterNode.Spec.Taints = []corev1.Taint{
		{
			Key:    corev1.TaintNodeUnschedulable,
			Effect: corev1.TaintEffectNoSchedule,
		},
	}
	tests := []struct {
		name                  string
		infraObject           *configv1.Infrastructure
		errString             string
		err                   bool
		operandPod            *corev1.Pod
		node                  *corev1.Node
		guardExists           bool
		guardPod              *corev1.Pod
		createConditionalFunc func() (bool, bool, error)
	}{
		{
			name: "Operand pod missing",
			infraObject: &configv1.Infrastructure{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.InfrastructureStatus{
					ControlPlaneTopology: configv1.SingleReplicaTopologyMode,
				},
			},
			errString:  "Missing operand on node master1",
			err:        true,
			operandPod: nil,
			node:       fakeMasterNode("master1"),
		},
		{
			name: "Operand pod missing .Status.PodIP",
			infraObject: &configv1.Infrastructure{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.InfrastructureStatus{
					ControlPlaneTopology: configv1.SingleReplicaTopologyMode,
				},
			},
			errString: "Missing PodIP in operand operand1 on node master1",
			err:       true,
			operandPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "operand1",
					Namespace: "test",
					Labels:    map[string]string{"app": "operand"},
				},
				Spec: corev1.PodSpec{
					NodeName: "master1",
				},
				Status: corev1.PodStatus{},
			},
			node: fakeMasterNode("master1"),
		},
		{
			name: "Operand guard pod created",
			infraObject: &configv1.Infrastructure{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.InfrastructureStatus{
					ControlPlaneTopology: configv1.SingleReplicaTopologyMode,
				},
			},
			operandPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "operand1",
					Namespace: "test",
					Labels:    map[string]string{"app": "operand"},
				},
				Spec: corev1.PodSpec{
					NodeName: "master1",
				},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
			node:        fakeMasterNode("master1"),
			guardExists: true,
		},
		{
			name: "Master node not schedulable",
			infraObject: &configv1.Infrastructure{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.InfrastructureStatus{
					ControlPlaneTopology: configv1.SingleReplicaTopologyMode,
				},
			},
			operandPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "operand1",
					Namespace: "test",
					Labels:    map[string]string{"app": "operand"},
				},
				Spec: corev1.PodSpec{
					NodeName: "master1",
				},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
			node:        unschedulableMasterNode,
			guardExists: false,
		},
		{
			name: "Operand guard pod deleted",
			infraObject: &configv1.Infrastructure{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.InfrastructureStatus{
					ControlPlaneTopology: configv1.HighlyAvailableTopologyMode,
				},
			},
			operandPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "operand1",
					Namespace: "test",
					Labels:    map[string]string{"app": "operand"},
				},
				Spec: corev1.PodSpec{
					NodeName: "master1",
				},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
			guardPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      getGuardPodName("operand", "master1"),
					Namespace: "test",
					Labels:    map[string]string{"app": "guard"},
				},
				Spec: corev1.PodSpec{
					Hostname: "guard-master1",
					NodeName: "master1",
				},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
			node: fakeMasterNode("master1"),
		},
		{
			name: "Guard pod is not pending nor running",
			infraObject: &configv1.Infrastructure{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.InfrastructureStatus{
					ControlPlaneTopology: configv1.SingleReplicaTopologyMode,
				},
			},
			operandPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "operand1",
					Namespace: "test",
					Labels:    map[string]string{"app": "operand"},
				},
				Spec: corev1.PodSpec{
					NodeName: "master1",
				},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
			guardPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      getGuardPodName("operand", "master1"),
					Namespace: "test",
					Labels:    map[string]string{"app": "guard"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Image: "",
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Host: "1.1.1.1",
										Port: intstr.FromInt(99999),
										Path: "",
									},
								},
							},
						},
					},
					Hostname: getGuardPodHostname("test", "master1"),
					NodeName: "master1",
				},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
					Phase: corev1.PodSucceeded,
				},
			},
			node:        fakeMasterNode("master1"),
			guardExists: true,
		},
		{
			name: "Conditional return precheckSucceeded is false",
			infraObject: &configv1.Infrastructure{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.InfrastructureStatus{
					ControlPlaneTopology: configv1.SingleReplicaTopologyMode,
				},
			},
			createConditionalFunc: func() (bool, bool, error) { return false, false, nil },
			operandPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "operand1",
					Namespace: "test",
					Labels:    map[string]string{"app": "operand"},
				},
				Spec: corev1.PodSpec{
					NodeName: "master1",
				},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
			node:        fakeMasterNode("master1"),
			guardExists: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			if err := indexer.Add(test.infraObject); err != nil {
				t.Fatal(err.Error())
			}

			kubeClient := fake.NewSimpleClientset(test.node)
			if test.operandPod != nil {
				kubeClient.Tracker().Add(test.operandPod)
			}
			if test.guardPod != nil {
				kubeClient.Tracker().Add(test.guardPod)
			}
			kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute)
			eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{})

			informer := FakeInfrastructureInformer{
				Informer_: FakeInfrastructureSharedInformer{
					HasSynced_: true,
				},
				Lister_: FakeInfrastructureLister{
					InfrastructureLister_: configlistersv1.NewInfrastructureLister(indexer),
				},
			}

			createConditionalFunc := staticcontrollercommon.NewIsSingleNodePlatformFn(informer)
			if test.createConditionalFunc != nil {
				createConditionalFunc = test.createConditionalFunc
			}

			ctrl := &GuardController{
				targetNamespace:         "test",
				podResourcePrefix:       "operand",
				operatorName:            "operator",
				operandPodLabelSelector: labels.Set{"app": "operand"}.AsSelector(),
				readyzPort:              "99999",
				nodeLister:              kubeInformers.Core().V1().Nodes().Lister(),
				podLister:               kubeInformers.Core().V1().Pods().Lister(),
				podGetter:               kubeClient.CoreV1(),
				pdbGetter:               kubeClient.PolicyV1(),
				pdbLister:               kubeInformers.Policy().V1().PodDisruptionBudgets().Lister(),
				installerPodImageFn:     getInstallerPodImageFromEnv,
				createConditionalFunc:   createConditionalFunc,
			}

			ctx, cancel := context.WithCancel(context.TODO())
			defer cancel()

			kubeInformers.Start(ctx.Done())
			kubeInformers.WaitForCacheSync(ctx.Done())

			err := ctrl.sync(ctx, FakeSyncContext{recorder: eventRecorder})
			if test.err {
				if test.errString != err.Error() {
					t.Errorf("%s: expected error message %q, got %q", test.name, test.errString, err)
				}
			} else {
				if test.guardExists {
					p, err := kubeClient.CoreV1().Pods("test").Get(ctx, getGuardPodName("operand", "master1"), metav1.GetOptions{})
					if err != nil {
						t.Errorf("%s: unexpected error: %v", test.name, err)
					} else {
						probe := p.Spec.Containers[0].ReadinessProbe.HTTPGet
						if probe == nil {
							t.Errorf("%s: missing ReadinessProbe in the guard", test.name)
						}
						if probe.Host != test.operandPod.Status.PodIP {
							t.Errorf("%s: expected %q host in ReadinessProbe in the guard, got %q instead", test.name, test.operandPod.Status.PodIP, probe.Host)
						}

						if probe.Port.IntValue() != 99999 {
							t.Errorf("%s: unexpected port in ReadinessProbe in the guard, expected 99999, got %v instead", test.name, probe.Port.IntValue())
						}

						if p.Status.Phase != "" {
							t.Errorf("%s: unexpected pod status: %v, expected no status set", test.name, p.Status.Phase)
						}
					}
				} else {
					_, err := kubeClient.CoreV1().Pods("test").Get(ctx, getGuardPodName("operand", "master1"), metav1.GetOptions{})
					if !apierrors.IsNotFound(err) {
						t.Errorf("%s: expected 'pods \"%v\" not found' error, got %q instead", test.name, getGuardPodName("operand", "master1"), err)
					}
				}
			}
		})
	}
}

// change a guard pod based on a change of an operand ip address (to update the readiness probe)
func TestRenderGuardPodPortChanged(t *testing.T) {
	infraObject := &configv1.Infrastructure{
		ObjectMeta: v1.ObjectMeta{
			Name: "cluster",
		},
		Status: configv1.InfrastructureStatus{
			ControlPlaneTopology: configv1.SingleReplicaTopologyMode,
		},
	}
	operandPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "operand1",
			Namespace: "test",
			Labels:    map[string]string{"app": "operand"},
		},
		Spec: corev1.PodSpec{
			NodeName: "master1",
		},
		Status: corev1.PodStatus{
			PodIP: "2.2.2.2",
		},
	}
	guardPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getGuardPodName("operand", "master1"),
			Namespace: "test",
			Labels:    map[string]string{"app": "guard"},
		},
		Spec: corev1.PodSpec{
			Hostname: "guard-master1",
			NodeName: "master1",
			Containers: []corev1.Container{
				{
					Image: "",
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Host: "1.1.1.1",
								Port: intstr.FromInt(99998),
								Path: "readyzpath",
							},
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			PodIP: "1.1.1.1",
		},
	}

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(infraObject); err != nil {
		t.Fatal(err.Error())
	}

	kubeClient := fake.NewSimpleClientset(fakeMasterNode("master1"), operandPod, guardPod)
	kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute)
	eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{})

	informer := FakeInfrastructureInformer{
		Informer_: FakeInfrastructureSharedInformer{
			HasSynced_: true,
		},
		Lister_: FakeInfrastructureLister{
			InfrastructureLister_: configlistersv1.NewInfrastructureLister(indexer),
		},
	}

	ctrl := &GuardController{
		targetNamespace:         "test",
		podResourcePrefix:       "operand",
		operandPodLabelSelector: labels.Set{"app": "operand"}.AsSelector(),
		operatorName:            "operator",
		readyzPort:              "99999",
		readyzEndpoint:          "readyz",
		nodeLister:              kubeInformers.Core().V1().Nodes().Lister(),
		podLister:               kubeInformers.Core().V1().Pods().Lister(),
		podGetter:               kubeClient.CoreV1(),
		pdbGetter:               kubeClient.PolicyV1(),
		pdbLister:               kubeInformers.Policy().V1().PodDisruptionBudgets().Lister(),
		installerPodImageFn:     getInstallerPodImageFromEnv,
		createConditionalFunc:   staticcontrollercommon.NewIsSingleNodePlatformFn(informer),
	}

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	kubeInformers.Start(ctx.Done())
	kubeInformers.WaitForCacheSync(ctx.Done())

	// expected to pass
	if err := ctrl.sync(ctx, FakeSyncContext{recorder: eventRecorder}); err != nil {
		t.Fatal(err.Error())
	}

	// check the probe.Host is the same as the operand ip address
	p, err := kubeClient.CoreV1().Pods("test").Get(ctx, getGuardPodName("operand", "master1"), metav1.GetOptions{})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	} else {
		probe := p.Spec.Containers[0].ReadinessProbe.HTTPGet
		originalProbe := guardPod.Spec.Containers[0].ReadinessProbe.HTTPGet
		if probe == nil {
			t.Errorf("missing ReadinessProbe in the guard")
		}
		if probe.Host != operandPod.Status.PodIP {
			t.Errorf("expected %q host in ReadinessProbe in the guard, got %q instead", operandPod.Status.PodIP, probe.Host)
		}

		// The port is expected to be set to 99999 by the guard controller
		if probe.Port.IntValue() != 99999 {
			t.Errorf("unexpected port in ReadinessProbe in the guard, expected %q, got %q instead", ctrl.readyzPort, probe.Port.IntValue())
		}
		// The port is expected to be different from the one initially set in the guard pod readiness probe
		if originalProbe.Port.IntValue() == probe.Port.IntValue() {
			t.Errorf("unexpected port in ReadinessProbe in the guard, expected it to be different from %q, got %q", originalProbe.Port.IntValue(), probe.Port.IntValue())
		}

		// The path is expected to be set to healthz by the guard controller
		if probe.Path != ctrl.readyzEndpoint {
			t.Errorf("unexpected path in ReadinessProbe in the guard, expected %q, got %q instead", ctrl.readyzEndpoint, probe.Path)
		}
		// The path is expected to be different from the one initially set in the guard pod readiness probe
		if probe.Path == originalProbe.Path {
			t.Errorf("unexpected path in ReadinessProbe in the guard, expected it to be differenf from %q, got %q", originalProbe.Path, probe.Path)
		}
	}
}

func TestGuardPodTemplate(t *testing.T) {
	const partitioningAnnotation = "target.workload.openshift.io/management"

	tests := []struct {
		name  string
		check func(pod *corev1.Pod) error
	}{
		{
			// https://github.com/openshift/enhancements/blob/master/enhancements/workload-partitioning/management-workload-partitioning.md
			name: fmt.Sprintf("has the %q annotation set correctly", partitioningAnnotation),
			check: func(pod *corev1.Pod) error {
				expectedValue := "{\"effect\": \"PreferredDuringScheduling\"}"
				annotation := pod.GetAnnotations()[partitioningAnnotation]
				if annotation == "" {
					return fmt.Errorf("expected %q annotation to be set", partitioningAnnotation)
				}
				if annotation != expectedValue {
					return fmt.Errorf("expected %q annotation to be set to %q, got %q", partitioningAnnotation, expectedValue, annotation)
				}
				return nil
			},
		},
	}

	pod := resourceread.ReadPodV1OrDie(podTemplate)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.check(pod); err != nil {
				t.Error(err)
			}
		})
	}
}
