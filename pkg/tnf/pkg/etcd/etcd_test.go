package etcd

import (
	"context"
	"testing"

	v1 "github.com/openshift/api/operator/v1"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

type args struct {
	ctx            context.Context
	operatorClient operatorversionedclient.Interface
	kubeClient     *kubefake.Clientset
}

func TestRemoveStaticContainer(t *testing.T) {
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name:    "sets ReadyForEtcdContainerRemoval condition",
			args:    getArgs(),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := RemoveStaticContainer(tt.args.ctx, tt.args.operatorClient, tt.args.kubeClient); (err != nil) != tt.wantErr {
				t.Errorf("RemoveStaticContainer() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify that the job condition was set
			job, err := tt.args.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(tt.args.ctx, tools.JobTypeSetup.GetNameLabelValue(), metav1.GetOptions{})
			if err != nil {
				t.Errorf("failed to get TNF setup job: %v", err)
				return
			}

			found := false
			for _, condition := range job.Status.Conditions {
				if string(condition.Type) == jobs.ReadyForEtcdContainerRemovalCondition && condition.Status == corev1.ConditionTrue {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Expected ReadyForEtcdContainerRemoval condition to be set to True")
			}
		})
	}
}

func getArgs() args {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	etcd := &v1.Etcd{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Spec: v1.EtcdSpec{
			StaticPodOperatorSpec: v1.StaticPodOperatorSpec{
				OperatorSpec: v1.OperatorSpec{
					UnsupportedConfigOverrides: runtime.RawExtension{},
				},
			},
		},
	}

	// Create TNF setup job
	tnfJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tools.JobTypeSetup.GetNameLabelValue(),
			Namespace: operatorclient.TargetNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": tools.JobTypeSetup.GetNameLabelValue(),
			},
		},
		Status: batchv1.JobStatus{},
	}

	fakeOperatorClient := operatorfake.NewClientset(etcd)
	fakeKubeClient := kubefake.NewSimpleClientset(tnfJob)

	return args{
		ctx:            ctx,
		operatorClient: fakeOperatorClient,
		kubeClient:     fakeKubeClient,
	}
}
