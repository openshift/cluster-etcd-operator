package flock

import (
	"os"
	"sync"
)

// FLock a type that supports file locking to coordinate work between processes
type FLock struct {
	locker sync.Mutex

	lockFilePath string
	lockedFile   *os.File
}

// New instantiates *FLock on the given path.
// The path must point to a file not a directory.
// The file doesn't have to exist prior to calling this method.
// It will be creating on the first call to TryLock method.
func New(filePath string) *FLock {
	return &FLock{locker: sync.Mutex{}, lockFilePath: filePath}
}
