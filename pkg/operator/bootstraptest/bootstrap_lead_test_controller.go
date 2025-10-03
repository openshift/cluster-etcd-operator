package bootstraptest

import (
	"context"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"k8s.io/klog/v2"
)

type BootstrapTestController struct {
	etcdClient etcdcli.EtcdClient
}

func NewBootstrapTestController(
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
) factory.Controller {

	evc := eventRecorder.WithComponentSuffix("bootstrap-test-controller")
	c := &BootstrapTestController{
		etcdClient: etcdClient,
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	return factory.New().ResyncEvery(2*time.Minute).WithSync(syncer.Sync).
		ToController("BootstrapTestController", evc)
}

func (c *BootstrapTestController) sync(ctx context.Context, _ factory.SyncContext) error {

	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return err
	}

	var hasBootstrap bool
	var bootstrapMember *etcdserverpb.Member
	for _, member := range members {
		if member.Name == "etcd-bootstrap" {
			hasBootstrap = true
			bootstrapMember = member
			break
		}
	}

	if hasBootstrap {
		klog.Warningf("TEST_ONLY moving leader to the bootstrap member")
		moved, err := ceohelpers.MoveLeaderToAnotherMember(ctx, c.etcdClient, bootstrapMember, members)
		if err != nil {
			return err
		}
		if moved {
			klog.Warningf("TEST_ONLY successfully moved to the bootstrap member")
		} else {
			klog.Warningf("TEST_ONLY failed to move to the bootstrap member")
		}
	}

	return nil
}
