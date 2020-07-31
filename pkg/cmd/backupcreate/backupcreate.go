package backupcreate

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog"
)

type backupOptions struct {
	endpoint  string
	configDir string
	dataDir   string
	backupDir string
	errOut    io.Writer
}

func NewBackupCreateCommand(errOut io.Writer) *cobra.Command {
	backupOpts := &backupOptions{
		errOut: errOut,
	}
	backupCmd := &cobra.Command{
		Use:   "backup-create",
		Short: "generates backup files",
		Run: func(backupCmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if backupCmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(backupOpts.errOut, err.Error())
				}
			}

			must(backupOpts.Run)
		},
	}
	backupOpts.AddFlags(backupCmd.Flags())
	return backupCmd
}

func (r *backupOptions) AddFlags(fs *pflag.FlagSet) {
	fs.Set("logtostderr", "true")
	fs.StringVar(&r.configDir, "config-dir", "/etc/kubernetes", "Path to the kubernetes config directory")
	fs.StringVar(&r.backupDir, "backup-dir", "/backup", "Path to the directory where the backup is generated")
}

func (r *backupOptions) Run() error {
	if err := backup(r); err != nil {
		klog.Errorf("run: backup failed: %v", err)
	}
	klog.Infof("config-dir is: %s", r.configDir)
	return nil
}

func backup(r *backupOptions) error {
	//TODO dateString should be unified across the pod.
	dateString := time.Now().Format("2006-01-02_150405")
	outputArchive := "static_kuberesources_" + dateString + ".tar.gz"

	tarFile, err := os.OpenFile(filepath.Join(r.backupDir, outputArchive), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	defer tarFile.Close()
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}
	if err := createTarball(r.configDir, tarFile); err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	return nil
}

func createTarball(src string, writers ...io.Writer) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("Unable to tar files - %v", err.Error())
	}

	mw := io.MultiWriter(writers...)

	gzw := gzip.NewWriter(mw)
	defer gzw.Close()

	tw := tar.NewWriter(gzw)
	defer tw.Close()

	return filepath.Walk(src, func(file string, fi os.FileInfo, err error) error {
		// return on any error
		if err != nil {
			return err
		}

		// return on non-regular files (thanks to [kumo](https://medium.com/@komuw/just-like-you-did-fbdd7df829d3) for this suggested update)
		if !fi.Mode().IsRegular() {
			return nil
		}

		// create a new dir/file header
		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}

		// update the name to correctly reflect the desired destination when untaring
		header.Name = strings.TrimPrefix(strings.Replace(file, src, "", -1), string(filepath.Separator))

		// write the header
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		// open files for taring
		f, err := os.Open(file)
		if err != nil {
			return err
		}

		// copy file data into tar writer
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}

		// manually close here after each file operation; defering would cause each file close
		// to wait until all operations have completed.
		f.Close()

		return nil
	})
}

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
