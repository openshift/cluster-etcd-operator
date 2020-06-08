package rollbackcopy

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func createTarball(tarballFilePath string, filePaths []string, prefixTrim string) error {
	file, err := os.OpenFile(tarballFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return errors.New(fmt.Sprintf("Could not create tarball file '%s', got error '%s'", tarballFilePath, err.Error()))
	}
	defer file.Close()

	gzipWriter := gzip.NewWriter(file)
	defer gzipWriter.Close()

	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	if prefixTrim != "" && !strings.HasSuffix(prefixTrim, "/") {
		prefixTrim += "/"
	}

	for _, filePath := range filePaths {
		err := addFileToTarWriter(filePath, tarWriter, prefixTrim)
		if err != nil {
			return errors.New(fmt.Sprintf("Could not add file '%s', to tarball, got error '%s'", filePath, err.Error()))
		}
	}

	return nil
}

// Private methods

func addFileToTarWriter(src string, tarWriter *tar.Writer, prefixTrim string) error {

	return filepath.Walk(src, func(file string, fi os.FileInfo, err error) error {

		// return on any error
		if err != nil {
			return fmt.Errorf("addFileToTarWriter failed: %w", err)
		}

		// return on non-regular files
		if !fi.Mode().IsRegular() {
			return nil
		}

		header := &tar.Header{
			Name:    file,
			Size:    fi.Size(),
			Mode:    int64(fi.Mode()),
			ModTime: fi.ModTime(),
		}

		// update the name to correctly reflect the desired destination when untaring
		header.Name = strings.TrimPrefix(file, prefixTrim)

		// write the header
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("addFileToTarWriter: WriteHeader failed: %w", err)
		}

		// open files for taring
		f, err := os.Open(file)
		if err != nil {
			return fmt.Errorf("addFileToTarWriter: file open failed: %w", err)
		}

		// copy file data into tar writer
		if _, err := io.Copy(tarWriter, f); err != nil {
			return fmt.Errorf("addFileToTarWriter: file write failed: %w", err)
		}

		// manually close here after each file operation; defering would cause each file close
		// to wait until all operations have completed.
		f.Close()

		return nil
	})

}
