package prune

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"
)

type PruneOptions struct {
	MaxEligibleRevision int
	ProtectedRevisions  []int

	ResourceDir   string
	CertDir       string
	StaticPodName string
}

func NewPruneOptions() *PruneOptions {
	return &PruneOptions{}
}

func NewPrune() *cobra.Command {
	o := NewPruneOptions()

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Prune static pod installer revisions",
		Run: func(cmd *cobra.Command, args []string) {
			klog.V(1).Info(cmd.Flags())
			klog.V(1).Info(spew.Sdump(o))

			if err := o.Validate(); err != nil {
				klog.Fatal(err)
			}
			if err := o.Run(); err != nil {
				klog.Fatal(err)
			}
		},
	}

	o.AddFlags(cmd.Flags())

	return cmd
}

func (o *PruneOptions) AddFlags(fs *pflag.FlagSet) {
	fs.IntVar(&o.MaxEligibleRevision, "max-eligible-revision", o.MaxEligibleRevision, "highest revision ID to be eligible for pruning")
	fs.IntSliceVar(&o.ProtectedRevisions, "protected-revisions", o.ProtectedRevisions, "list of revision IDs to preserve (not delete)")
	fs.StringVar(&o.ResourceDir, "resource-dir", o.ResourceDir, "directory for all files supporting the static pod manifest")
	fs.StringVar(&o.StaticPodName, "static-pod-name", o.StaticPodName, "name of the static pod")
	fs.StringVar(&o.CertDir, "cert-dir", o.CertDir, "directory for all certs")
}

func (o *PruneOptions) Validate() error {
	if len(o.ResourceDir) == 0 {
		return fmt.Errorf("--resource-dir is required")
	}
	if o.MaxEligibleRevision == 0 {
		return fmt.Errorf("--max-eligible-revision is required")
	}
	if len(o.StaticPodName) == 0 {
		return fmt.Errorf("--static-pod-name is required")
	}

	return nil
}

func (o *PruneOptions) Run() error {
	files, err := os.ReadDir(o.ResourceDir)
	if err != nil {
		return err
	}

	for _, file := range files {
		// If the file is not a resource directory...
		if !file.IsDir() {
			continue
		}
		// And doesn't match our static pod prefix...
		if !strings.HasPrefix(file.Name(), o.StaticPodName) {
			continue
		}

		// Split file name to get just the integer revision ID
		fileSplit := strings.Split(file.Name(), o.StaticPodName+"-")
		revisionID, err := strconv.Atoi(fileSplit[len(fileSplit)-1])
		if err != nil {
			return err
		}

		// And is not protected...
		if protected := slices.Contains(o.ProtectedRevisions, revisionID); protected {
			continue
		}
		// And is less than or equal to the maxEligibleRevisionID
		if revisionID > o.MaxEligibleRevision {
			continue
		}

		err = os.RemoveAll(path.Join(o.ResourceDir, file.Name()))
		if err != nil {
			return err
		}
	}

	// prune any temporary certificate files
	// we do create temporary files to atomically "write" various certificates to disk
	// usually, these files are short-lived because they are immediately renamed, the following loop removes old/unused/dangling files
	//
	// the temporary files have the following form:
	//  /etc/kubernetes/static-pod-resources/kube-apiserver-certs/configmaps/control-plane-node-kubeconfig/kubeconfig.tmp753375784
	//  /etc/kubernetes/static-pod-resources/kube-apiserver-certs/secrets/service-network-serving-certkey/tls.key.tmp643092404
	if len(o.CertDir) == 0 {
		return nil
	}

	// If the cert dir does not exist, do nothing.
	// The dir will get eventually created by an installer pod.
	if _, err := os.Stat(path.Join(o.ResourceDir, o.CertDir)); os.IsNotExist(err) {
		klog.Infof("Skipping %s as it does not exist", path.Join(o.ResourceDir, o.CertDir))
		return nil
	}

	return filepath.Walk(path.Join(o.ResourceDir, o.CertDir),
		func(filePath string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			// info.Name() gives just a filename like tls.key or tls.key.tmp643092404
			if !strings.Contains(info.Name(), ".tmp") {
				return nil
			}
			if time.Now().Sub(info.ModTime()) > 30*time.Minute {
				klog.Infof("Removing %s, the last time it was modified was %v", filePath, info.ModTime())
				if err := os.RemoveAll(filePath); err != nil {
					return err
				}
			}
			return nil
		},
	)
}
