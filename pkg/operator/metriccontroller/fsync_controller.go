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
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog"

	configclientv1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
)

type FSyncController struct {
	secretClient corev1client.SecretsGetter
	infraClient  configclientv1.InfrastructuresGetter
}

func NewFSyncController(operatorClient operatorv1helpers.OperatorClient, infraClient configclientv1.InfrastructuresGetter, secretGetter corev1client.SecretsGetter, recorder events.Recorder) factory.Controller {
	c := &FSyncController{
		secretClient: secretGetter,
		infraClient:  infraClient,
	}
	return factory.New().ResyncEvery(1*time.Minute).WithSync(c.sync).ToController("FSyncController", recorder)
}

func (c *FSyncController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	client, err := getPrometheusClient(ctx, c.secretClient)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	// First check how many leader changes we see in last 15 minutes
	etcdLeaderChangesResult, _, err := client.Query(ctx, "increase((max by (job) (etcd_server_leader_changes_seen_total) or 0*absent(etcd_server_leader_changes_seen_total))[5m:1m])", time.Now())
	if err != nil {
		return err
	}
	leaderChanges := etcdLeaderChangesResult.(model.Vector)[0].Value
	klog.V(4).Infof("Etcd leader changes increase in last 5m: %s", leaderChanges)

	// Do nothing if there are no leader changes
	if leaderChanges == 0.0 {
		return nil
	}

	// Capture etcd disk metrics as we detected excessive etcd leader changes
	etcdWalFsyncResult, _, err := client.QueryRange(ctx, "histogram_quantile(0.99, rate(etcd_disk_wal_fsync_duration_seconds_bucket[5m]))", prometheusv1.Range{
		Start: time.Now().Add(-1 * time.Hour),
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

	values := []string{}
	for _, s := range matrix {
		if _, ok := s.Metric["pod"]; !ok {
			klog.Warningf("No pod label found in metric: %+v", s.Metric)
			continue
		}
		values = append(values, fmt.Sprintf("%s=%s", s.Metric["pod"], s.Values[0].Value))
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
	syncCtx.Recorder().Warningf("EtcdLeaderChangeMetrics", "Detected leader change increase of %s over 5 minutes on %q; disk metrics are: %s", leaderChanges, platformType, strings.Join(values, ","))

	// TODO: Consider Degraded condition here.

	return nil
}
