package cmd

import (
	"context"
	"strings"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"

	tnf "github.com/openshift/cluster-etcd-operator/pkg/tnf/setup"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
)

func NewTnfSetup() *cobra.Command {
	cmd := controllercmd.
		NewControllerCommandConfig("tnf-setup", version.Get(), tnf.RunTnfSetup, clock.RealClock{}).
		WithEventRecorderOptions(TnfEventRecorderOptions()).
		NewCommandWithContext(context.Background())
	cmd.Use = "tnf-setup"
	cmd.Short = "Start the Two node fencing setup"

	return cmd
}

// TnfEventRecorderOptions is a very strict correlator policy to avoid spamming etcd/apiserver with duplicated events
func TnfEventRecorderOptions() record.CorrelatorOptions {
	return record.CorrelatorOptions{
		// only allow the same event five times in 10m
		MaxEvents:            5,
		MaxIntervalInSeconds: 600,
		BurstSize:            5,         // default: 25 (change allows a single source to send 5 events about object per minute)
		QPS:                  1. / 300., // default: 1/300 (change allows refill rate to 1 new event every 300s)
		KeyFunc: func(event *corev1.Event) (aggregateKey string, localKey string) {
			return strings.Join([]string{event.Type, event.Reason, event.Message}, "_"), event.Message
		},
	}
}
