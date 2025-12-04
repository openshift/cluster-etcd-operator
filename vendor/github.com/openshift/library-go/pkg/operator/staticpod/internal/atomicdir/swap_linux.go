//go:build linux

package atomicdir

import (
	"golang.org/x/sys/unix"
)

// swap can be used to exchange two directories atomically.
func swap(firstDir, secondDir string) error {
	// Renameat2 can be used to exchange two directories atomically when RENAME_EXCHANGE flag is specified.
	// The paths to be exchanged can be specified in multiple ways:
	//
	//   * You can specify a file descriptor and a relative path to that descriptor.
	//   * You can specify an absolute path, in which case the file descriptor is ignored.
	//
	// We use AT_FDCWD special file descriptor so that when any of the paths is relative,
	// it's relative to the current working directory.
	//
	// For more details, see `man renameat2` as that is the associated C library function.
	return unix.Renameat2(unix.AT_FDCWD, firstDir, unix.AT_FDCWD, secondDir, unix.RENAME_EXCHANGE)
}
