package jobs

import (
	"context"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	opv1 "github.com/openshift/api/operator/v1"
	fakeconfig "github.com/openshift/client-go/config/clientset/versioned/fake"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	coreinformers "k8s.io/client-go/informers"
	fakecore "k8s.io/client-go/kubernetes/fake"
	clocktesting "k8s.io/utils/clock/testing"
)

const (
	infraConfigName  = "cluster"
	jobName          = "testjob"
	defaultClusterID = "ID1234"
	controllerName   = "TestJobController"
	operandName      = "dummy-controller"
	operandNamespace = "openshift-test"
)

// fakeOperatorInstance is a fake Operator instance that  fullfils the OperatorClient interface.
type fakeOperatorInstance struct {
	metav1.ObjectMeta
	Spec   opv1.OperatorSpec
	Status opv1.OperatorStatus
}

// Infrastructure
func makeInfra() *configv1.Infrastructure {
	return &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name:      infraConfigName,
			Namespace: v1.NamespaceAll,
		},
		Status: configv1.InfrastructureStatus{
			InfrastructureName: defaultClusterID,
		},
	}
}

func makeFakeManifest() []byte {
	return []byte(`
apiVersion: batch/v1
kind: Job
metadata:
  labels:
    app.kubernetes.io/name: testjob
  namespace: openshift-test
  name: testjob
spec:
  template:
    metadata:
      annotations:
        openshift.io/required-scc: "privileged"
    spec:
      containers:
        - name: testjob
          image: testimage
          imagePullPolicy: IfNotPresent
          command: [ "test", "arg" ]
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 128Mi
          securityContext:
            privileged: true
            allowPrivilegeEscalation: true
      hostIPC: false
      hostNetwork: false
      hostPID: true
      serviceAccountName: test-manager
      terminationGracePeriodSeconds: 10
      restartPolicy: Never
    backoffLimit: 3`)
}

type operatorModifier func(instance *fakeOperatorInstance) *fakeOperatorInstance

func makeFakeOperatorInstance(modifiers ...operatorModifier) *fakeOperatorInstance {
	instance := &fakeOperatorInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster",
			Generation: 0,
		},
		Spec: opv1.OperatorSpec{
			ManagementState: opv1.Managed,
		},
		Status: opv1.OperatorStatus{},
	}
	for _, modifier := range modifiers {
		instance = modifier(instance)
	}
	return instance
}

func TestJobCreation(t *testing.T) {
	// Initialize
	coreClient := fakecore.NewSimpleClientset()
	coreInformerFactory := coreinformers.NewSharedInformerFactory(coreClient, 0 /*no resync */)
	initialInfras := []runtime.Object{makeInfra()}
	configClient := fakeconfig.NewSimpleClientset(initialInfras...)
	configInformerFactory := configinformers.NewSharedInformerFactory(configClient, 0)
	configInformer := configInformerFactory.Config().V1().Infrastructures().Informer()
	configInformer.GetIndexer().Add(initialInfras[0])
	driverInstance := makeFakeOperatorInstance()
	fakeOperatorClient := v1helpers.NewFakeOperatorClientWithObjectMeta(&driverInstance.ObjectMeta, &driverInstance.Spec, &driverInstance.Status, nil /*triggerErr func*/)
	var optionalInformers []factory.Informer
	optionalInformers = append(optionalInformers, configInformer)
	var optionalJobHooks []JobHookFunc
	controller := NewJobController(
		controllerName,
		makeFakeManifest(),
		events.NewInMemoryRecorder(operandName, clocktesting.NewFakePassiveClock(time.Now())),
		fakeOperatorClient,
		coreClient,
		coreInformerFactory.Batch().V1().Jobs(),
		optionalInformers,
		optionalJobHooks...,
	)

	// Act
	err := controller.Sync(context.TODO(), factory.NewSyncContext(controllerName, events.NewInMemoryRecorder("dummy-controller", clocktesting.NewFakePassiveClock(time.Now()))))
	if err != nil {
		t.Fatalf("sync() returned unexpected error: %v", err)
	}

	// Assert
	_, err = coreClient.BatchV1().Jobs(operandNamespace).Get(context.TODO(), jobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get Job %s: %v", jobName, err)
	}

	// TODO add more tests, e.g. for operator conditions based on job status
}
