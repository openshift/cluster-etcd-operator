package etcd

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/openshift/api/operator/v1"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	"github.com/openshift/client-go/operator/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type args struct {
	ctx            context.Context
	operatorClient operatorversionedclient.Interface
}

func TestRemoveStaticContainer(t *testing.T) {
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name:    "default",
			args:    getArgs(),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := RemoveStaticContainer(tt.args.ctx, tt.args.operatorClient); (err != nil) != tt.wantErr {
				t.Errorf("RemoveStaticContainer() error = %v, wantErr %v", err, tt.wantErr)
			}
			etcd, err := tt.args.operatorClient.OperatorV1().Etcds().Get(tt.args.ctx, "cluster", metav1.GetOptions{})
			if err != nil {
				t.Errorf("failed to get etcd: %v", err)
			}
			overrides := string(etcd.Spec.UnsupportedConfigOverrides.Raw)
			if !strings.Contains(overrides, `"useUnsupportedUnsafeEtcdContainerRemoval":true`) {
				t.Errorf("expected useUnsupportedUnsafeEtcdContainerRemoval got %s", overrides)
			}
			if !strings.Contains(overrides, `"useExternalEtcdSupport":true`) {
				t.Errorf("expected useExternalEtcdSupport got %s", overrides)
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

	fakeOperatorClient := fake.NewClientset(etcd)

	return args{
		ctx:            ctx,
		operatorClient: fakeOperatorClient,
	}
}
