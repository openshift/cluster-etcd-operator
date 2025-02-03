package backuprestore

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	prune "github.com/openshift/cluster-etcd-operator/pkg/cmd/prune-backups"

	"k8s.io/klog/v2"

	"github.com/robfig/cron/v3"
	"github.com/spf13/cobra"
)

const (
	BackupVolume      = "/var/lib/etcd-auto-backup"
	etcdCtlKeyName    = "ETCDCTL_KEY"
	etcdCtlCertName   = "ETCDCTL_CERT"
	etcdCtlCACertName = "ETCDCTL_CACERT"
	nodeNameEnvVar    = "NODE_NAME"
)

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

type backupRunner interface {
	runBackup(*backupOptions, *prune.PruneOpts) error
}

type backupRunnerImpl struct{}

func (b backupRunnerImpl) runBackup(backupOpts *backupOptions, pruneOpts *prune.PruneOpts) error {
	dateString := time.Now().Format("2006-01-02_150405")
	backupOpts.backupDir = path.Join(BackupVolume, dateString)
	err := backup(backupOpts)
	if err != nil {
		return err
	}

	err = pruneOpts.Run()
	if err != nil {
		return err
	}

	return nil
}

type backupServer struct {
	schedule     string
	timeZone     string
	nodeName     string
	enabled      bool
	cronSchedule cron.Schedule
	backupOptions
	prune.PruneOpts
}

func NewBackupServer(ctx context.Context) *cobra.Command {
	backupSrv := &backupServer{
		backupOptions: backupOptions{errOut: os.Stderr},
		PruneOpts: prune.PruneOpts{
			RetentionType: "None",
			BackupPath:    BackupVolume,
		},
	}

	cmd := &cobra.Command{
		Use:   "backup-server",
		Short: "Backs up a snapshot of etcd database and static pod resources without config",
		Run: func(cmd *cobra.Command, args []string) {
			if err := backupSrv.Validate(); err != nil {
				klog.Fatal(err)
			}
			if err := backupSrv.Run(ctx); err != nil {
				klog.Fatal(err)
			}
		},
	}

	backupSrv.AddFlags(cmd)
	return cmd
}

func (b *backupServer) AddFlags(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.BoolVar(&b.enabled, "enabled", false, "enable backup server")
	fs.StringVar(&b.schedule, "schedule", "", "schedule specifies the cron schedule to run the backup")
	fs.StringVar(&b.timeZone, "timezone", "", "timezone specifies the timezone of the cron schedule to run the backup")

	b.backupOptions.AddFlags(fs)
	b.PruneOpts.AddFlags(cmd)
}

func (b *backupServer) Validate() error {
	if !b.enabled {
		klog.Infof("backup-server is disabled")
		return nil
	}

	if err := b.validateNameNode(); err != nil {
		return err
	}

	if err := b.constructEnvVars(); err != nil {
		klog.Infof("error constructing envVars: [%v]", err)
		return err
	}

	cronSchedule, err := cron.ParseStandard(b.schedule)
	if err != nil {
		return fmt.Errorf("error parsing backup schedule %v: %w", b.schedule, err)
	}
	b.cronSchedule = cronSchedule

	b.backupOptions.backupDir = BackupVolume
	err = b.backupOptions.Validate()
	if err != nil {
		return fmt.Errorf("error validating backup %v: %w", b.backupOptions, err)
	}

	err = b.PruneOpts.Validate()
	if err != nil {
		return fmt.Errorf("error validating prune args %v: %w", b.PruneOpts, err)
	}
	return nil
}

func (b *backupServer) Run(ctx context.Context) error {
	// handle teardown
	cCtx, cancel := signal.NotifyContext(ctx, shutdownSignals...)
	defer cancel()

	if b.enabled {
		bck := backupRunnerImpl{}
		err := b.scheduleBackup(cCtx, bck)
		if err != nil {
			klog.Infof("error running etcd backup: %v", err)
			return err
		}
	}

	<-ctx.Done()
	return nil
}

func (b *backupServer) scheduleBackup(ctx context.Context, bck backupRunner) error {
	ticker := time.NewTicker(time.Until(b.cronSchedule.Next(time.Now())))
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			err := bck.runBackup(&b.backupOptions, &b.PruneOpts)
			if err != nil {
				klog.Errorf("error running backup: %v", err)
				return err
			}
			ticker.Reset(time.Until(b.cronSchedule.Next(time.Now())))
		case <-ctx.Done():
			return nil
		}
	}
}

func (b *backupServer) validateNameNode() error {
	nodeNameEnv := os.Getenv(nodeNameEnvVar)
	if len(nodeNameEnv) == 0 {
		return fmt.Errorf("[%v] environment variable is empty", nodeNameEnvVar)
	}
	b.nodeName = nodeNameEnv
	return nil
}

func (b *backupServer) constructEnvVars() error {
	etcdCtlKeyVal := strings.Replace("/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.key", nodeNameEnvVar, b.nodeName, -1)
	err := os.Setenv(etcdCtlKeyName, etcdCtlKeyVal)
	if err != nil {
		return fmt.Errorf("error exporting [%v]: val is [%v]: %w", etcdCtlKeyName, etcdCtlKeyVal, err)
	}

	etcdCtlCertVal := strings.Replace("/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.crt", nodeNameEnvVar, b.nodeName, -1)
	err = os.Setenv(etcdCtlCertName, etcdCtlCertVal)
	if err != nil {
		return fmt.Errorf("error writing [%v]: val is [%v]: %w", etcdCtlCertName, etcdCtlCertVal, err)
	}

	etcdCtlCACertVal := "/etc/kubernetes/static-pod-certs/configmaps/etcd-all-bundles/server-ca-bundle.crt"
	err = os.Setenv(etcdCtlCACertName, etcdCtlCACertVal)
	if err != nil {
		return fmt.Errorf("error writing [%v]: val is [%v]: %w", etcdCtlCACertName, etcdCtlCACertVal, err)
	}

	return nil
}
