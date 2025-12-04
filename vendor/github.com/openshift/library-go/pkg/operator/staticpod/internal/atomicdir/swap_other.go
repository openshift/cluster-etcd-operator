//go:build !linux

package atomicdir

import "errors"

// swap can be used to exchange two directories atomically.
//
// This function is only implemented for Linux and returns an error on other platforms.
func swap(firstDir, secondDir string) error {
	return errors.New("swap is not supported on this platform")
}
