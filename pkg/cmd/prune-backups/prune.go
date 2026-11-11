package prune_backups

import (
	"errors"
	goflag "flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/spf13/cobra"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
)

const (
	// BasePath for Backups we assume we have full ownership over the root folder at /etc/kubernetes/cluster-backup
	// for backup-server we use another path for storing backups (i.e. /var/lib/etcd-auto-backup)
	// for tests we will change this to a tmp directory
	BasePath = backuphelpers.BackupPathMount

	RetentionTypeNone   = "None"
	RetentionTypeSize   = "RetentionSize"
	RetentionTypeNumber = "RetentionNumber"
	RetentionTypeFiles  = "Files"
)

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
	Files              []string
	BackupPath         string
}

func NewPruneCommand() *cobra.Command {
	opts := PruneOpts{
		RetentionType: "None",
	}
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
	flagSet.StringVar(&r.RetentionType, "type", r.RetentionType, "Which kind of retention to execute, can either be None, RetentionNumber, RetentionSize, or Files.")
	// the defaults are zero for validation, we inject the real defaults from the periodic backup controller
	flagSet.IntVar(&r.MaxNumberOfBackups, "maxNumberOfBackups", 0, "how many backups to keep when type=RetentionNumber")
	flagSet.IntVar(&r.MaxSizeOfBackupsGb, "maxSizeOfBackupsGb", 0, "how many gigabytes of backups to keep when type=RetentionSize")
	flagSet.StringSliceVar(&r.Files, "files", r.Files, "the name of files to delete when type=Files. If directory is passed, only directly nested backup files will be deleted.")
	flagSet.StringVar(&r.BackupPath, "backupPath", BasePath, "path for backups to be pruned")

	// adding klog flags to tune verbosity better
	gfs := goflag.NewFlagSet("", goflag.ExitOnError)
	klog.InitFlags(gfs)
	cmd.Flags().AddGoFlagSet(gfs)
}

func (r *PruneOpts) Validate() error {
	switch r.RetentionType {
	case RetentionTypeNumber:
		if r.MaxNumberOfBackups < 1 {
			return fmt.Errorf("unexpected amount of backups [%d] found, expected at least 1", r.MaxNumberOfBackups)
		}

		if r.MaxSizeOfBackupsGb != 0 {
			return fmt.Errorf("unexpected argument [MaxSizeOfBackupsGb] found while using %s", RetentionTypeNumber)
		}

	case RetentionTypeSize:
		if r.MaxSizeOfBackupsGb < 1 {
			return fmt.Errorf("unexpected size of backups [%d]gb found, expected at least 1", r.MaxSizeOfBackupsGb)
		}

		if r.MaxNumberOfBackups != 0 {
			return fmt.Errorf("unexpected argument [MaxNumberOfBackups] found while using %s", RetentionTypeSize)
		}
	case RetentionTypeFiles:
		if len(r.Files) == 0 {
			return fmt.Errorf("expected at least 1 file to be specified for type=Files")
		}
	case RetentionTypeNone:
		return nil
	default:
		return fmt.Errorf("unknown retention type: [%s]", r.RetentionType)
	}

	return nil
}

func (r *PruneOpts) Run() error {
	switch r.RetentionType {
	case RetentionTypeNone:
		klog.Infof("nothing to do, retention type is none")
		return nil
	case RetentionTypeSize:
		return retainBySizeGb(r.MaxSizeOfBackupsGb, r.BackupPath)
	case RetentionTypeNumber:
		return retainByNumber(r.MaxNumberOfBackups, r.BackupPath)
	case RetentionTypeFiles:
		return deleteByName(r.Files, r.BackupPath)
	}

	return nil
}

func retainBySizeGb(sizeInGb int, backupPath string) error {
	folders, err := listAllBackupFolders(backupPath)
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
			toRemove = append(toRemove, path.Join(backupPath, f.name))
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

func retainByNumber(maxNumBackups int, backupPath string) error {
	folders, err := listAllBackupFolders(backupPath)
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
		bPath := path.Join(backupPath, f.name)
		klog.Infof("deleting [%s]...", bPath)
		err = os.RemoveAll(bPath)
		if err != nil {
			return fmt.Errorf("error while removing [%s]: %w", bPath, err)
		}
	}
	klog.Infof("pruning successful")
	return nil
}

func deleteByName(files []string, backupPath string) error {
	errs := []error{}
	parentDirs := map[string]struct{}{}

	// Iterate files, validating they match expected backup files and expanding directories into the backup files they contain.
	backupFiles := make([]string, 0, len(files))
	for _, file := range files {
		if !strings.HasPrefix(file, "/") {
			file = path.Join(backupPath, file)
		}

		fileInfo, err := os.Stat(file)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("error while checking [%s]: %w", file, err))
			}
			continue
		}

		if !backuphelpers.IsBackupFilePath(file) {
			if !fileInfo.IsDir() {
				errs = append(errs, fmt.Errorf("expected file matching one of [snapshot_<datetime>.db, snapshot_<datetime>.db.part, static_kuberesources_<datetime>.tar.gz], got: %v", file))
				continue
			}
		}

		if fileInfo.IsDir() {
			files, _, err := backuphelpers.ListBackupFiles(file)
			if err != nil {
				errs = append(errs, fmt.Errorf("error walking directory [%s]: %w", file, err))
			}
			backupFiles = append(backupFiles, files...)
		} else {
			backupFiles = append(backupFiles, file)
		}
	}

	// Delete backup files
	for _, file := range backupFiles {
		if err := os.Remove(file); err != nil {
			errs = append(errs, fmt.Errorf("error while removing file [%s]: %w", file, err))
		}
		parentDirs[path.Dir(file)] = struct{}{}
	}

	// Delete empty backup dirs
	for dir := range parentDirs {
		if empty, err := isEmpty(dir); err != nil {
			errs = append(errs, fmt.Errorf("error checking if parent dir [%s] is empty: %w", dir, err))
		} else if empty {
			if err := os.Remove(dir); err != nil {
				errs = append(errs, fmt.Errorf("error while removing empty parent dir [%s]: %w", dir, err))
			}
		}
	}

	return utilerrors.NewAggregate(errs)
}

func listAllBackupFolders(backupPath string) (backupDirStats, error) {
	var stats []backupDirStat

	dir, err := os.ReadDir(backupPath)
	if err != nil {
		return nil, fmt.Errorf("could not list dir [%s]: %w", dir, err)
	}

	for _, d := range dir {
		if !d.IsDir() {
			continue
		}

		var dirSize int64
		var latestModTime time.Time
		err := fs.WalkDir(os.DirFS(path.Join(backupPath, d.Name())), ".", func(path string, d fs.DirEntry, err error) error {
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
	if b[j].modTime.Equal(b[i].modTime) {
		return strings.Compare(b[j].name, b[i].name) < 0
	}
	return b[j].modTime.Before(b[i].modTime)
}

func (b backupDirStats) Swap(i, j int) {
	tmp := b[i]
	b[i] = b[j]
	b[j] = tmp
}

func isEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()

	// Read up to 1 entry names from the directory.
	_, err = f.Readdirnames(1)

	// If the error is EOF, it means there are 0 entries (the directory is empty).
	if errors.Is(err, io.EOF) {
		return true, nil
	}

	// If err is nil, it means at least one entry was read (not empty).
	// If err is another error, return it.
	return false, err
}
