package operator

import (
	"context"
	"fmt"
	"github.com/openshift/cluster-etcd-operator/pkg/operator"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/client-go/tools/record"
	"net/http"
	"strings"
)

func NewOperator() *cobra.Command {
	cmd := controllercmd.
		NewControllerCommandConfig("openshift-cluster-etcd-operator", version.Get(), operator.RunOperator).
		WithEventRecorderOptions(EtcdOperatorCorrelatorOptions()).
		WithHealthChecks(healthz.NamedCheck("controller-aliveness", func(_ *http.Request) error {
			if !operator.AlivenessChecker.Alive() {
				return fmt.Errorf("found unhealthy aliveness check, returning error")
			}
			return nil
		})).
		NewCommandWithContext(context.Background())
	cmd.Use = "operator"
	cmd.Short = "Start the Cluster etcd Operator"

	return cmd
}

// EtcdOperatorCorrelatorOptions is a very strict correlator policy to avoid spamming etcd/apiserver with duplicated events
func EtcdOperatorCorrelatorOptions() record.CorrelatorOptions {
	return record.CorrelatorOptions{
		// only allow the same event ten times in 10m
		MaxEvents:            10,
		MaxIntervalInSeconds: 300,
		BurstSize:            1,         // default: 25 (change allows a single source to send 1 event about object per minute)
		QPS:                  1. / 300., // default: 1/300 (change allows refill rate to 1 new event every 300s)
		KeyFunc: func(event *corev1.Event) (aggregateKey string, localKey string) {
			return strings.Join([]string{event.Type, event.Reason, event.Message}, "_"), event.Message
		},
	}
}
