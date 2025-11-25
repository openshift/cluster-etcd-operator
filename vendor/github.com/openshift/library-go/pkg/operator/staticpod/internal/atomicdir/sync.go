package atomicdir

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"

	"github.com/openshift/library-go/pkg/operator/staticpod/internal/atomicdir/types"
)

// Sync can be used to atomically synchronize target directory with the given file content map.
// This is done by populating a staging directory, then atomically swapping it with the target directory.
// This effectively means that any extra files in the target directory are pruned.
//
// The staging directory needs to be explicitly specified. It is initially created using os.MkdirAll with targetDirPerm.
// It is then populated using files with filePerm. Once the atomic swap is performed, the staging directory
// (which is now the original target directory) is removed.
func Sync(targetDir string, targetDirPerm os.FileMode, stagingDir string, files map[string]types.File) error {
	return sync(&realFS, targetDir, targetDirPerm, stagingDir, files)
}

type fileSystem struct {
	MkdirAll        func(path string, perm os.FileMode) error
	RemoveAll       func(path string) error
	WriteFile       func(name string, data []byte, perm os.FileMode) error
	SwapDirectories func(dirA, dirB string) error
}

var realFS = fileSystem{
	MkdirAll:        os.MkdirAll,
	RemoveAll:       os.RemoveAll,
	WriteFile:       os.WriteFile,
	SwapDirectories: swap,
}

// sync prepares a tmp directory and writes all files into that directory.
// Then it atomically swap the tmp directory for the target one.
// This is currently implemented as really atomically swapping directories.
//
// The same goal of atomic swap could be implemented using symlinks much like AtomicWriter does in
// https://github.com/kubernetes/kubernetes/blob/v1.34.0/pkg/volume/util/atomic_writer.go#L58
// The reason we don't do that is that we already have a directory populated and watched that needs to we swapped.
// In other words, it's for compatibility reasons. And if we were to migrate to the symlink approach,
// we would anyway need to atomically turn the current data directory into a symlink.
// This would all just increase complexity and require atomic swap on the OS level anyway.
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

		if err := fs.WriteFile(fullFilename, file.Content, file.Perm); err != nil {
			return fmt.Errorf("failed writing %q: %w", fullFilename, err)
		}
	}

	klog.Infof("Atomically swapping staging directory %q with target directory %q ...", stagingDir, targetDir)
	if err := fs.SwapDirectories(targetDir, stagingDir); err != nil {
		return fmt.Errorf("failed swapping target directory %q with staging directory %q: %w", targetDir, stagingDir, err)
	}
	return
}
