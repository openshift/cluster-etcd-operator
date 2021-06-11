package staticpod

import (
	"fmt"
	"os"
	"path/filepath"

	"io/ioutil"
)

func WriteFileAtomic(content []byte, filePerms os.FileMode, fullFilename string) error {
	tmpFile, err := writeTemporaryFile(content, filePerms, fullFilename)
	if err != nil {
		return err
	}

	return os.Rename(tmpFile, fullFilename)
}

func writeTemporaryFile(content []byte, filePerms os.FileMode, fullFilename string) (string, error) {
	contentDir := filepath.Dir(fullFilename)
	filename := filepath.Base(fullFilename)
	tmpfile, err := ioutil.TempFile(contentDir, fmt.Sprintf("%s.tmp", filename))
	if err != nil {
		return "", err
	}
	defer tmpfile.Close()
	if err := tmpfile.Chmod(filePerms); err != nil {
		return "", err
	}
	if _, err := tmpfile.Write(content); err != nil {
		return "", err
	}
	return tmpfile.Name(), nil
}
