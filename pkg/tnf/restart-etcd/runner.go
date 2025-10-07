package restartetcd

import (
	"context"

	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
)

func RunEtcdRestart() error {
	klog.Info("Running TNF etcd restart")
	ctx := context.Background()

	err := pcs.RestartEtcd(ctx)
	if err != nil {
		return err
	}

	klog.Infof("Etcd restart done!")

	return nil
}
