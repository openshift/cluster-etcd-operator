package metricshandler

import (
	"context"
	"fmt"
	"time"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"k8s.io/klog"

	"github.com/openshift/library-go/pkg/operator/events"
)

func EtcdFSyncHandler(ctx context.Context, recorder events.Recorder, client prometheusv1.API) error {
	result, warns, err := client.QueryRange(ctx, "histogram_quantile(0.99, rate(etcd_disk_wal_fsync_duration_seconds_bucket[5m]))", prometheusv1.Range{
		Start: time.Now(),
		End:   time.Now(),
		Step:  1 * time.Second,
	})
	if err != nil {
		return err
	}
	if len(warns) > 0 {
		klog.Warningf("Prometheus warnings received: %#v", warns)
	}
	matrix, ok := result.(model.Matrix)
	if !ok {
		return fmt.Errorf("result is not Matrix")
	}
	for _, s := range matrix {
		// TODO: Do something useful with these values
		// Sample output:
		//
		// client_test.go:46: etcd-ip-10-0-139-103.us-east-2.compute.internal=0.0019909198813056386
		// client_test.go:46: etcd-ip-10-0-153-19.us-east-2.compute.internal=0.0019937764350453175
		// client_test.go:46: etcd-ip-10-0-170-79.us-east-2.compute.internal=0.001993969696969697
		//
		klog.Infof("%s=%s", s.Metric["pod"], s.Values[0].Value)
	}
	return nil
}
