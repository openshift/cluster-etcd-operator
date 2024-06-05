package startupmonitor

import (
	"fmt"
	"io/fs"
	"os"
	"time"

	kerrors "k8s.io/apimachinery/pkg/util/errors"
)

type fakeIO struct {
	StatFn                func(string) (os.FileInfo, error)
	StatFnCounter         int
	ExpectedStatFnCounter int

	SymlinkFn                func(string, string) error
	SymlinkFnCounter         int
	ExpectedSymlinkFnCounter int

	RemoveFn                func(string) error
	RemoveFnCounter         int
	ExpectedRemoveFnCounter int

	ReadFileFn                func(string) ([]byte, error)
	ReadFileFnCounter         int
	ExpectedReadFileFnCounter int

	ReadDirFn                func(string) ([]fs.DirEntry, error)
	ReadDirFnCounter         int
	ExpectedReadDirFnCounter int

	WriteFileFn                func(filename string, data []byte, perm fs.FileMode) error
	WriteFileFnCounter         int
	ExpectedWriteFileFnCounter int
}

func (f *fakeIO) Symlink(oldname string, newname string) error {
	f.SymlinkFnCounter++
	if f.SymlinkFn != nil {
		return f.SymlinkFn(oldname, newname)
	}
	return nil
}

func (f *fakeIO) Stat(path string) (os.FileInfo, error) {
	f.StatFnCounter++
	if f.StatFn != nil {
		return f.StatFn(path)
	}
	return nil, nil
}

func (f *fakeIO) Remove(path string) error {
	f.RemoveFnCounter++
	if f.RemoveFn != nil {
		return f.RemoveFn(path)
	}
	return nil
}

func (f *fakeIO) ReadFile(filename string) ([]byte, error) {
	f.ReadFileFnCounter++
	if f.ReadFileFn != nil {
		return f.ReadFileFn(filename)
	}

	return nil, nil
}

func (f *fakeIO) ReadDir(dirname string) ([]fs.DirEntry, error) {
	f.ReadDirFnCounter++
	if f.ReadDirFn != nil {
		return f.ReadDirFn(dirname)
	}
	return nil, nil
}

func (f *fakeIO) WriteFile(filename string, data []byte, perm fs.FileMode) error {
	f.WriteFileFnCounter++
	if f.WriteFileFn != nil {
		return f.WriteFileFn(filename, data, perm)
	}
	return nil
}

func (f *fakeIO) Validate() error {
	var errs []error
	if f.SymlinkFnCounter != f.ExpectedSymlinkFnCounter {
		errs = append(errs, fmt.Errorf("unexpected SymlinkFnCounter %d, expected %d", f.SymlinkFnCounter, f.ExpectedSymlinkFnCounter))
	}

	if f.StatFnCounter != f.ExpectedStatFnCounter {
		errs = append(errs, fmt.Errorf("unexpected StatFnCounter %d, expected %d", f.StatFnCounter, f.ExpectedStatFnCounter))
	}

	if f.RemoveFnCounter != f.ExpectedRemoveFnCounter {
		errs = append(errs, fmt.Errorf("unexpected RemoveFnCounter %d, expected %d", f.RemoveFnCounter, f.ExpectedRemoveFnCounter))
	}

	if f.ReadFileFnCounter != f.ExpectedReadFileFnCounter {
		errs = append(errs, fmt.Errorf("unexpected ReadFileFnCounter %d, expected %d", f.ReadFileFnCounter, f.ExpectedReadFileFnCounter))
	}

	if f.ReadDirFnCounter != f.ExpectedReadDirFnCounter {
		errs = append(errs, fmt.Errorf("unexpected ReadDirFnCounter %d, expected %d", f.ReadDirFnCounter, f.ExpectedReadDirFnCounter))
	}

	if f.WriteFileFnCounter != f.ExpectedWriteFileFnCounter {
		errs = append(errs, fmt.Errorf("unexpected WriteFileFnCounter %d, expected %d", f.WriteFileFnCounter, f.ExpectedWriteFileFnCounter))
	}

	return kerrors.NewAggregate(errs)
}

type fakeFile string

func (f fakeFile) Name() string               { return string(f) }
func (f fakeFile) Size() int64                { return 0 }
func (f fakeFile) Mode() fs.FileMode          { return fs.ModeAppend }
func (f fakeFile) ModTime() time.Time         { return time.Unix(0, 0) }
func (f fakeFile) IsDir() bool                { return false }
func (f fakeFile) Sys() interface{}           { return nil }
func (f fakeFile) Info() (os.FileInfo, error) { return f, nil }
func (f fakeFile) Type() fs.FileMode          { return f.Mode() }

type fakeDir string

func (f fakeDir) Name() string               { return string(f) }
func (f fakeDir) Size() int64                { return 0 }
func (f fakeDir) Mode() fs.FileMode          { return fs.ModeDir | 0500 }
func (f fakeDir) ModTime() time.Time         { return time.Unix(0, 0) }
func (f fakeDir) IsDir() bool                { return true }
func (f fakeDir) Sys() interface{}           { return nil }
func (f fakeDir) Info() (os.FileInfo, error) { return f, nil }
func (f fakeDir) Type() fs.FileMode          { return f.Mode() }
