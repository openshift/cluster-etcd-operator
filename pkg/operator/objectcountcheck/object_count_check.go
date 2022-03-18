package objectcountcheck

import (
	"context"
	"time"

	"github.com/davecgh/go-spew/spew"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

func New(client etcdcli.EtcdClient, operatorClient v1helpers.StaticPodOperatorClient, prefixes []string, eventRecorder events.Recorder) (factory.Controller, error) {
	syncCtx := factory.NewSyncContext("ObjectCountCheck", eventRecorder.WithComponentSuffix("object-count-check"))
	c := &controller{
		etcdClient: client,
		prefixes:   prefixes,
	}
	return factory.New().WithSyncContext(syncCtx).ResyncEvery(time.Minute).WithSync(c.sync).ToController("ObjectCountCheck", syncCtx.Recorder()), nil
}

type controller struct {
	etcdClient etcdcli.EtcdClient
	prefixes   []string
}

func (c *controller) sync(ctx context.Context, context factory.SyncContext) error {
	counts, err := c.etcdClient.GetObjectCounts(ctx, c.prefixes)
	if err != nil {
		return err
	}

	// TODO: do something useful with the object counters here
	klog.Infof("object counts:\n%s", spew.Sdump(counts))
	return nil
}
