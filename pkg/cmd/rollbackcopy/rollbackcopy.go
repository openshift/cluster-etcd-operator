package rollbackcopy

import (
	"fmt"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"io"
	"k8s.io/klog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type rollbackCopyOpts struct {
	checkLeadershipInterval time.Duration
	rollbackCopyInterval    time.Duration
	configDir               string
	errOut                  io.Writer
}

const (
	leaderShipCheckInterval time.Duration = 5 * time.Minute
	rollbackSaveInterval    time.Duration = 60 * time.Minute
)

func NewRollbackCopy(errOut io.Writer) *cobra.Command {
	rollbackCopyOpts := &rollbackCopyOpts{
		checkLeadershipInterval: leaderShipCheckInterval,
		rollbackCopyInterval:    rollbackSaveInterval,
		errOut:                  errOut,
	}
	cmd := &cobra.Command{
		Use:   "rollbackcopy",
		Short: "Periodically save snapshot and resources every hour, useful for a rollback",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(rollbackCopyOpts.errOut, err.Error())
				}
			}

			must(rollbackCopyOpts.Run)
		},
	}
	rollbackCopyOpts.AddFlags(cmd.Flags())
	return cmd
}

func (r *rollbackCopyOpts) AddFlags(fs *pflag.FlagSet) {
	fs.Set("logtostderr", "true")
	fs.StringVar(&r.configDir, "config-dir", "/etc/kubernetes", "Dir containing kubernetes resources")
}

func (r *rollbackCopyOpts) Run() error {
	localEtcdName := os.Getenv("ETCD_NAME")
	ossignal := make(chan os.Signal, 1)
	signal.Notify(ossignal, os.Interrupt, syscall.SIGTERM)

	initial := make(chan struct{}, 1)
	initial <- struct{}{}

outerLoop:
	for {
		// Block until we're the leader, checking every 5 mins
		leaderCheckTicker := time.NewTicker(r.checkLeadershipInterval)
	leaderWait:
		for {
			select {
			case <-ossignal:
				leaderCheckTicker.Stop()
				break outerLoop
			case <-initial:
				if leader, err := checkLeadership(localEtcdName); err != nil {
					klog.Errorf("run: leadershipCheck failed %w", err)
					continue leaderWait
				} else if !leader {
					klog.Info("run: member is NOT the leader.")
					continue leaderWait
				}
				break leaderWait
			case <-leaderCheckTicker.C:
				if leader, err := checkLeadership(localEtcdName); err != nil {
					klog.Errorf("run: leadershipCheck failed %w", err)
					continue leaderWait
				} else if !leader {
					klog.Info("run: member is NOT the leader.")
					continue leaderWait
				}
				break leaderWait
			}
		}

		leaderCheckTicker.Stop()

		// We are leader. Take a back up.
		klog.Info("run: member IS the leader")
		if err := backup(r.configDir); err != nil {
			klog.Errorf("backup: %v", err)
			continue outerLoop
		}

		// Try to back up every hour while we're the leader
		backupTicker := time.NewTicker(r.rollbackCopyInterval)
	backupLoop:
		for {
			select {
			case <-ossignal:
				backupTicker.Stop()
				break outerLoop
			case <-backupTicker.C:
				if leader, err := checkLeadership(localEtcdName); err != nil {
					klog.Errorf("run: leadershipCheck failed: %w", err)
					break backupLoop
				} else if !leader {
					klog.Info("run: member is NOT the leader anymore.")
					break backupLoop
				}
				klog.Info("run: member IS still the leader")
				if err := backup(r.configDir); err != nil {
					klog.Errorf("run: backup failed: %v", err)
					break backupLoop
				}
			}
		}
		backupTicker.Stop()
	}

	return nil
}
