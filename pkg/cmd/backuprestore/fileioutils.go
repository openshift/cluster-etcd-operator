package backuprestore

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func dirExists(dirname string) (bool, error) {
	info, err := os.Stat(dirname)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("dirExists: %s. Error=%w", dirname, err)
	}
	return info.IsDir(), nil
}

func fileExists(filename string) (bool, error) {
	stat, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("fileExists: %s. Error=%w", filename, err)
	}
	return !stat.IsDir(), nil
}

func fileCopy(src, dst string) (int64, error) {
	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return 0, err
	}

	if !sourceFileStat.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", src)
	}

	source, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer destination.Close()
	nBytes, err := io.Copy(destination, source)
	return nBytes, err
}

func findTheLatestRevision(dir, filePrefix string, isDir bool) (string, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return "", err
	}

	var modTime time.Time
	var latest string
	found := false
	for _, f := range files {
		if f.IsDir() == isDir && strings.HasPrefix(f.Name(), filePrefix) {
			if f.ModTime().After(modTime) {
				modTime = f.ModTime()
				latest = f.Name()
				found = true
			}
		}
	}
	if !found {
		return "", fmt.Errorf("Not found any resources with file prefix %s", filePrefix)
	}
	return filepath.Join(dir, latest), nil
}

func checkAndCreateDir(dirName string) error {
	_, err := os.Stat(dirName)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("checkAndCreateDir failed: %w", err)
	}
	// If dirName already exists, empty it
	if err == nil {
		if err := emptyDirectory(dirName); err != nil {
			return fmt.Errorf("checkAndCreateDir failed to empty directory %s: %w", dirName, err)
		}
		return nil
	}
	if err := os.MkdirAll(dirName, os.ModePerm); err != nil {
		return fmt.Errorf("checkAndCreateDir failed: %w", err)
	}
	return nil
}

func emptyDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		err = os.RemoveAll(filepath.Join(dir, name))
		if err != nil {
			return err
		}
	}
	return nil
}
