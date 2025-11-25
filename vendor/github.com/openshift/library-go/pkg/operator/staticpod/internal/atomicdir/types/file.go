// Package types exists to avoid import cycles as it's imported by both atomicdir and atomicdir/testing.
package types

import "os"

// File represents file content together with the associated permissions.
type File struct {
	Content []byte
	Perm    os.FileMode
}
