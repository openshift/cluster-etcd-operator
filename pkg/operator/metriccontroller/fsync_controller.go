package metriccontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	operatorv1 "github.com/openshift/api/operator/v1"
	configclientv1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

type FSyncController struct {
	infraClient    configclientv1.InfrastructuresGetter
	operatorClient v1helpers.StaticPodOperatorClient
}

func NewFSyncController(infraClient configclientv1.InfrastructuresGetter,
	operatorClient v1helpers.StaticPodOperatorClient,
	recorder events.Recorder) factory.Controller {
	c := &FSyncController{
		infraClient:    infraClient,
		operatorClient: operatorClient,
	}
	return factory.New().ResyncEvery(1*time.Minute).WithSync(c.sync).ToController("FSyncController", recorder)
}

func (c *FSyncController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	transport, err := getTransport()
	if err != nil {
		return err
	}
	defer transport.CloseIdleConnections()

	client, err := getPrometheusClient(transport)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	// First check how many leader changes we see in last 5 minutes
	etcdLeaderChangesResult, _, err := client.Query(ctx, "max by (job) (increase(etcd_server_leader_changes_seen_total[5m]))", time.Now())
	if err != nil {
		return err
	}

	vector, ok := etcdLeaderChangesResult.(model.Vector)
	if !ok {
		return fmt.Errorf("unexpected type, expected Vector, got %T", vector)
	}
	if len(vector) == 0 {
		return fmt.Errorf("client query returned empty vector")
	}

	leaderChanges := vector[0].Value
	// Do nothing if there are no significant leader changes.
	if leaderChanges < 2.0 {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient,
			v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
				Type:   "FSyncControllerDegraded",
				Status: operatorv1.ConditionFalse,
				Reason: "AsExpected",
			}))
		return updateErr
	}

	klog.V(4).Infof("Etcd leader changes increase in last 5m: %s", leaderChanges)

	// Capture etcd disk metrics as we detected excessive etcd leader changes
	etcdWalFsyncResult, _, err := client.QueryRange(ctx, "histogram_quantile(0.99, rate(etcd_disk_wal_fsync_duration_seconds_bucket[5m]))", prometheusv1.Range{
		Start: time.Now().Add(-5 * time.Minute),
		End:   time.Now(),
		Step:  1 * time.Second,
	})
	if err != nil {
		return err
	}

	matrix, ok := etcdWalFsyncResult.(model.Matrix)
	if !ok {
		return fmt.Errorf("unexpected type, expected Matrix, got %T", matrix)
	}

	degradedMsg := ""
	values := []string{}
	for _, s := range matrix {
		if _, ok := s.Metric["pod"]; !ok {
			klog.Warningf("No pod label found in metric: %+v", s.Metric)
			continue
		}
		// If any value of the fsync disk is more than 3 seconds
		// we consider this not adequate hardware so we will go degraded.
		if s.Values[0].Value > 3.0 {
			degradedMsg = fmt.Sprintf("%s fsync duration value: %f, ", degradedMsg, s.Values[0].Value)
		}
		values = append(values, fmt.Sprintf("%s=%f", s.Metric["pod"], s.Values[0].Value))
	}

	infra, err := c.infraClient.Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var platformType string
	if infra.Status.PlatformStatus != nil {
		platformType = string(infra.Status.PlatformStatus.Type)
	}

	// Send warning event if excessive leader changes are detected. The event will include fsync disk metrics.
	syncCtx.Recorder().Warningf("EtcdLeaderChangeMetrics", "Detected leader change increase of %s over 5 minutes on %q; disk metrics are: %s. Most often this is as a result of inadequate storage or sometimes due to networking issues.", leaderChanges, platformType, strings.Join(values, ","))

	// TODO: In the future when alerts are more stable, query the following and go degraded based on alerts:
	// ALERTS{alertname=~\"etcd.+\", job=\"etcd\", alertstate="firing"}

	if degradedMsg != "" {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "FSyncControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "EtcdDiskMetricsExceededKnownThresholds",
			Message: fmt.Sprintf("etcd disk metrics exceeded known thresholds: %s", degradedMsg),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("FSyncControllerErrorUpdatingStatus", updateErr.Error())
		}
		return fmt.Errorf("etcd disk metrics exceeded known thresholds")
	}

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "FSyncControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))

	return updateErr
}
