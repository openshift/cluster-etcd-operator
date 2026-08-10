package main

import (
	"context"

	"github.com/spf13/cobra"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/certwatch"
)

func NewWatchCertsCommand() *cobra.Command {
	var certDir string

	cmd := &cobra.Command{
		Use:   "watch-certs",
		Short: "Watch CA bundle files and restart etcd when they change",
		Long: `Polls CA bundle certificate files on disk and restarts the local
etcd instance via Pacemaker when the files change. Sets restart_no_leave
before restarting to prevent force_new_cluster during CA rotation.

Runs as a long-lived process (DaemonSet). Requires hostPID and privileged
security context for nsenter access to Pacemaker and podman.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(context.Background())
			shutdownHandler := server.SetupSignalHandler()
			go func() {
				defer cancel()
				<-shutdownHandler
				klog.Info("Received SIGTERM or SIGINT signal, shutting down cert watcher")
			}()

			return certwatch.Run(ctx, certDir)
		},
	}

	cmd.Flags().StringVar(&certDir, "cert-dir", "", "Path to the CA bundle directory to watch (required)")
	cmd.MarkFlagRequired("cert-dir")

	return cmd
}
