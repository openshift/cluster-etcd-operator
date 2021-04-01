package disruptioncontroller

import (
	"context"
	"errors"
	"sync"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
)

const (
	syncDuration  = 2 * time.Minute
	retryDuration = 1 * time.Second
)

// DisruptionController observes the operand for disruption and events observations.
type DisruptionController struct {
	operatorClient v1helpers.OperatorClient
	etcdClient     etcdcli.EtcdClient
}

func NewDisruptionController(
	operatorClient v1helpers.OperatorClient,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &DisruptionController{
		operatorClient: operatorClient,
		etcdClient:     etcdClient,
	}
	return factory.New().ResyncEvery(syncDuration).WithInformers(
		operatorClient.Informer(),
	).WithSync(c.sync).ToController("DisruptionController", eventRecorder.WithComponentSuffix("disruption-controller"))
}

func (c *DisruptionController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.checkDisruption(ctx, syncCtx.Recorder())
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "DisruptionControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("DisruptionControllerUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "DisruptionControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *DisruptionController) checkDisruption(ctx context.Context, eventRecorder events.Recorder) error {
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), syncDuration)
	defer cancel()

	members, err := c.etcdClient.MemberList()
	if err != nil {
		return err
	}
	for _, member := range members {
		conn, err := c.etcdClient.Dial(member.ClientURLs[0])
		if err != nil {
			return err
		}
		wg.Add(1)
		go checkClientConn(ctx, conn, member.Name, eventRecorder, &wg)
	}
	wg.Wait()
	return nil
}

// checkClientConn periodically checks the connection state of etcd members
func checkClientConn(ctx context.Context, conn *grpc.ClientConn, memberName string, eventRecorder events.Recorder, wg *sync.WaitGroup) error {
	defer wg.Done()
	for {
	reset:
		select {
		case <-ctx.Done():
			klog.Info("Restarting connectivity checker to validate membership")
			conn.Close()
			return nil
		case <-time.After(retryDuration):
			state := conn.GetState()
			if conn.GetState() == connectivity.Ready {
				continue
			}
			// start tracking outage until Ready
			disruptionStart := time.Now()
			eventRecorder.Warningf("ConnectivityOutageDetected", "Connectivity outage detected: %s: state: %s\n", memberName, state.String())
			for {
				select {
				case <-ctx.Done(): // resync but notify current state
					eventRecorder.Warningf("ConnectivityOutagePaused", "Connectivity has not yet been restored after %s: %s: state: %s\n", time.Since(disruptionStart), memberName, state.String())
					conn.Close()
					return nil
				case <-time.After(retryDuration):
					state = conn.GetState()
					if state == connectivity.Ready {
						eventRecorder.Warningf("ConnectivityRestored", "Connectivity restored after %s: %s: state: %s\n", time.Since(disruptionStart), memberName, state.String())
						goto reset
					}
				}
			}
		}
	}
}
