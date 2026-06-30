package atomicdir

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"

	"github.com/openshift/library-go/pkg/operator/staticpod/internal/atomicdir/types"
	"github.com/openshift/library-go/pkg/operator/staticpod/internal/fsutil"
)

// Sync atomically and durably replaces the contents of targetDir with the given files.
// It writes files to a staging directory with fsync, then atomically swaps it with
// the target directory via renameat2(RENAME_EXCHANGE), fsyncing parent directories
// to ensure the swap is persisted. Extra files in targetDir are pruned.
// The old target directory (now at stagingDir) is removed after the swap.
func Sync(targetDir string, targetDirPerm os.FileMode, stagingDir string, files map[string]types.File) error {
	return sync(&realFS, targetDir, targetDirPerm, stagingDir, files)
}

type fileSystem struct {
	MkdirAll        func(path string, perm os.FileMode) error
	RemoveAll       func(path string) error
	WriteFileFsync  func(name string, data []byte, perm os.FileMode) error
	SwapDirectories func(dirA, dirB string) error
	Fsync           func(name string) error
}

var realFS = fileSystem{
	MkdirAll:        os.MkdirAll,
	RemoveAll:       os.RemoveAll,
	WriteFileFsync:  fsutil.WriteFileFsync,
	SwapDirectories: swap,
	Fsync:           fsutil.Fsync,
}

// sync writes files into the staging directory, then durably swaps it with the target.
// Each file write is individually fsynced (including its parent directory entry) via
// fs.WriteFileFsync. After the swap, parent directories are fsynced to persist the exchange.
//
// Note: the upstream Kubernetes AtomicWriter uses symlinks for atomic updates but does
// not fsync, leaving file data in the page cache with no crash durability guarantee:
// https://github.com/kubernetes/kubernetes/blob/v1.34.0/pkg/volume/util/atomic_writer.go#L58
// We use renameat2(RENAME_EXCHANGE) instead of symlinks because we need to swap an
// existing directory that is already being watched. Migrating to symlinks would still
// require an atomic swap at the OS level, adding complexity for no benefit.
func sync(fs *fileSystem, targetDir string, targetDirPerm os.FileMode, stagingDir string, files map[string]types.File) (retErr error) {
	klog.Infof("Ensuring target directory %q exists ...", targetDir)
	if err := fs.MkdirAll(targetDir, targetDirPerm); err != nil {
		return fmt.Errorf("failed creating target directory: %w", err)
	}

	klog.Infof("Creating staging directory %q ...", stagingDir)
	if err := fs.MkdirAll(stagingDir, targetDirPerm); err != nil {
		return fmt.Errorf("failed creating staging directory: %w", err)
	}
	defer func() {
		if err := fs.RemoveAll(stagingDir); err != nil {
			if retErr != nil {
				retErr = fmt.Errorf("failed removing staging directory %q: %w; previous error: %w", stagingDir, err, retErr)
				return
			}
			retErr = fmt.Errorf("failed removing staging directory %q: %w", stagingDir, err)
		}
	}()

	for filename, file := range files {
		// Make sure filename is a plain filename, not a path.
		// This also ensures the staging directory cannot be escaped.
		if filename != filepath.Base(filename) {
			return fmt.Errorf("filename cannot be a path: %q", filename)
		}

		fullFilename := filepath.Join(stagingDir, filename)
		klog.Infof("Writing file %q ...", fullFilename)

		if err := fs.WriteFileFsync(fullFilename, file.Content, file.Perm); err != nil {
			return fmt.Errorf("failed writing %q: %w", fullFilename, err)
		}
	}

	klog.Infof("Atomically swapping staging directory %q with target directory %q ...", stagingDir, targetDir)
	if err := fs.SwapDirectories(targetDir, stagingDir); err != nil {
		return fmt.Errorf("failed swapping target directory %q with staging directory %q: %w", targetDir, stagingDir, err)
	}

	// fsync parent directories to ensure the swap is durable on disk.
	// renameat2(RENAME_EXCHANGE) modifies parent directory entries, so the
	// parents must be fsynced to persist which inode each name points to.
	if err := fs.Fsync(filepath.Dir(targetDir)); err != nil {
		return fmt.Errorf("failed syncing parent directory of %q: %w", targetDir, err)
	}
	if err := fs.Fsync(filepath.Dir(stagingDir)); err != nil {
		return fmt.Errorf("failed syncing parent directory of %q: %w", stagingDir, err)
	}

	return
}
