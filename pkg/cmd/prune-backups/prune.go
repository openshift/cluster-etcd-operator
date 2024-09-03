package prune_backups

import (
	goflag "flag"
	"fmt"
	"github.com/spf13/cobra"
	"io/fs"
	"k8s.io/klog/v2"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// BasePath for Backups we assume we have full ownership over the root folder at /etc/kubernetes/cluster-backup
// for tests we will change this to a tmp directory
var BasePath = "/etc/kubernetes/cluster-backup/"

const RetentionTypeNone = "None"
const RetentionTypeSize = "RetentionSize"
const RetentionTypeNumber = "RetentionNumber"

type backupDirStats []backupDirStat

type backupDirStat struct {
	name      string
	sizeBytes int64
	modTime   time.Time
}

type PruneOpts struct {
	RetentionType      string
	MaxNumberOfBackups int
	MaxSizeOfBackupsGb int
}

func NewPruneCommand() *cobra.Command {
	opts := PruneOpts{RetentionType: "None"}
	cmd := &cobra.Command{
		Use:   "prune-backups",
		Short: "Prunes existing backups on the filesystem.",
		Run: func(cmd *cobra.Command, args []string) {
			defer klog.Flush()

			if err := opts.Validate(); err != nil {
				klog.Fatal(err)
			}
			if err := opts.Run(); err != nil {
				klog.Fatal(err)
			}
		},
	}

	opts.AddFlags(cmd)
	return cmd
}

func (r *PruneOpts) AddFlags(cmd *cobra.Command) {
	flagSet := cmd.Flags()
	flagSet.StringVar(&r.RetentionType, "type", r.RetentionType, "Which kind of retention to execute, can either be None, RetentionNumber or RetentionSize.")
	// the defaults are zero for validation, we inject the real defaults from the periodic backup controller
	flagSet.IntVar(&r.MaxNumberOfBackups, "maxNumberOfBackups", 0, "how many backups to keep when type=RetentionNumber")
	flagSet.IntVar(&r.MaxSizeOfBackupsGb, "maxSizeOfBackupsGb", 0, "how many gigabytes of backups to keep when type=RetentionSize")

	// adding klog flags to tune verbosity better
	gfs := goflag.NewFlagSet("", goflag.ExitOnError)
	klog.InitFlags(gfs)
	cmd.Flags().AddGoFlagSet(gfs)
}

func (r *PruneOpts) Validate() error {
	if r.RetentionType != RetentionTypeNone && r.RetentionType != RetentionTypeNumber && r.RetentionType != RetentionTypeSize {
		return fmt.Errorf("unknown retention type: [%s]", r.RetentionType)
	}

	if r.RetentionType == RetentionTypeNumber {
		if r.MaxNumberOfBackups < 1 {
			return fmt.Errorf("unexpected amount of backups [%d] found, expected at least 1", r.MaxNumberOfBackups)
		}

		if r.MaxSizeOfBackupsGb != 0 {
			return fmt.Errorf("unexpected argument [MaxSizeOfBackupsGb] found while using %s", RetentionTypeNumber)
		}

	} else if r.RetentionType == RetentionTypeSize {
		if r.MaxSizeOfBackupsGb < 1 {
			return fmt.Errorf("unexpected size of backups [%d]gb found, expected at least 1", r.MaxSizeOfBackupsGb)
		}

		if r.MaxNumberOfBackups != 0 {
			return fmt.Errorf("unexpected argument [MaxNumberOfBackups] found while using %s", RetentionTypeSize)
		}
	}

	return nil
}

func (r *PruneOpts) Run() error {
	if r.RetentionType == RetentionTypeNone {
		klog.Infof("nothing to do, retention type is none")
		return nil
	} else if r.RetentionType == RetentionTypeSize {
		return retainBySizeGb(r.MaxSizeOfBackupsGb)
	} else if r.RetentionType == RetentionTypeNumber {
		return retainByNumber(r.MaxNumberOfBackups)
	}

	return nil
}

func retainBySizeGb(sizeInGb int) error {
	folders, err := listAllBackupFolders()
	if err != nil {
		return err
	}

	sort.Sort(folders)

	// we keep the latest - up to sizeInGb folders around, the remainder is deleted
	// the newest backups are always found at the beginning of the list
	cutOffBytes := int64(1024 * 1024 * 1024 * sizeInGb)
	klog.Infof("configured cut-off bytes: %d (%dGiB)", cutOffBytes, sizeInGb)
	accBytes := int64(0)
	var toRemove []string
	for _, f := range folders {
		accBytes += f.sizeBytes
		if accBytes > cutOffBytes {
			toRemove = append(toRemove, path.Join(BasePath, f.name))
		} else {
			klog.Infof("retaining [%s], found [%d] bytes so far", f.name, accBytes)
		}
	}

	for _, f := range toRemove {
		klog.Infof("deleting [%s]...", f)
		err = os.RemoveAll(f)
		if err != nil {
			return fmt.Errorf("error while removing [%s]: %w", f, err)
		}
	}

	klog.Infof("pruning successful")
	return nil
}

func retainByNumber(maxNumBackups int) error {
	folders, err := listAllBackupFolders()
	if err != nil {
		return err
	}

	if len(folders) <= maxNumBackups {
		klog.Infof("numFolders=[%d] which is smaller or equal to requested [%d], no pruning necessary", len(folders), maxNumBackups)
		return nil
	}

	sort.Sort(folders)
	// the newest backups are always found at the beginning of the list
	for _, f := range folders[maxNumBackups:] {
		bPath := path.Join(BasePath, f.name)
		klog.Infof("deleting [%s]...", bPath)
		err = os.RemoveAll(bPath)
		if err != nil {
			return fmt.Errorf("error while removing [%s]: %w", bPath, err)
		}
	}
	klog.Infof("pruning successful")
	return nil
}

func listAllBackupFolders() (backupDirStats, error) {
	var stats []backupDirStat

	dir, err := os.ReadDir(BasePath)
	if err != nil {
		return nil, fmt.Errorf("could not list dir [%s]: %w", dir, err)
	}

	for _, d := range dir {
		if !d.IsDir() {
			continue
		}

		var dirSize int64
		var latestModTime time.Time
		err := fs.WalkDir(os.DirFS(path.Join(BasePath, d.Name())), ".", func(path string, d fs.DirEntry, err error) error {
			if !d.IsDir() {
				info, err := d.Info()
				if err != nil {
					return err
				}
				dirSize += info.Size()
				if latestModTime.Before(info.ModTime()) {
					latestModTime = info.ModTime()
				}
			}
			return nil
		})

		if err != nil {
			return nil, fmt.Errorf("could not recurse into dir [%s]: %w", d.Name(), err)
		}

		stats = append(stats, backupDirStat{
			name:      d.Name(),
			sizeBytes: dirSize,
			modTime:   latestModTime,
		})

	}

	klog.Infof("found backup folders: %v", stats)
	return stats, nil
}

func (s backupDirStat) String() string {
	return fmt.Sprintf("Name=[%s] SizeBytes=[%d] ModTime=[%s]", s.name, s.sizeBytes, s.modTime.String())
}

func (b backupDirStats) Len() int {
	return len(b)
}

// Less causes the slice to be sorted descending by modtime, the newest comes first
// in case the mod times are equal, we compare on the names in the same order.
func (b backupDirStats) Less(i, j int) bool {
	if b[j].modTime == b[i].modTime {
		return strings.Compare(b[j].name, b[i].name) < 0
	}
	return b[j].modTime.Before(b[i].modTime)
}

func (b backupDirStats) Swap(i, j int) {
	tmp := b[i]
	b[i] = b[j]
	b[j] = tmp
}
