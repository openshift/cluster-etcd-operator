package render

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/openshift/cluster-etcd-operator/pkg/cmd/render/options"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/v430_00_assets"
	"github.com/openshift/library-go/pkg/assets"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	yaml "gopkg.in/yaml.v2"
	"k8s.io/klog"
)

const (
	bootstrapVersion = "v4.3.0"
)

// renderOpts holds values to drive the render command.
type renderOpts struct {
	manifest options.ManifestOptions
	generic  options.GenericOptions

	errOut                   io.Writer
	etcdCAFile               string
	etcdMetricCAFile         string
	etcdDiscoveryDomain      string
	etcdImage                string
	clusterEtcdOperatorImage string
	etcdStaticResourcesDir   string
	etcdConfigDir            string
	setupEtcdEnvImage        string
	kubeClientAgentImage     string
}

// NewRenderCommand creates a render command.
func NewRenderCommand(errOut io.Writer) *cobra.Command {
	renderOpts := renderOpts{
		generic:                *options.NewGenericOptions(),
		manifest:               *options.NewManifestOptions("etcd"),
		errOut:                 errOut,
		etcdStaticResourcesDir: "/etc/kubernetes/static-pod-resources/etcd-member",
		etcdConfigDir:          "/etc/etcd",
	}
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render etcd bootstrap manifests, secrets and configMaps",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(renderOpts.errOut, err.Error())
				}
			}

			must(renderOpts.Validate)
			must(renderOpts.Complete)
			must(renderOpts.Run)
		},
	}

	renderOpts.AddFlags(cmd.Flags())

	return cmd
}

func (r *renderOpts) AddFlags(fs *pflag.FlagSet) {
	r.manifest.AddFlags(fs, "etcd")
	r.generic.AddFlags(fs)

	fs.StringVar(&r.etcdCAFile, "etcd-ca", r.etcdCAFile, "path to etcd CA certificate")
	fs.StringVar(&r.etcdMetricCAFile, "etcd-metric-ca", r.etcdMetricCAFile, "path to etcd metric CA certificate")
	fs.StringVar(&r.etcdImage, "manifest-etcd-image", r.etcdImage, "etcd manifest image")
	fs.StringVar(&r.clusterEtcdOperatorImage, "manifest-cluster-etcd-operator-image", r.clusterEtcdOperatorImage, "cluster-etcd-operator manifest image")
	fs.StringVar(&r.kubeClientAgentImage, "manifest-kube-client-agent-image", r.kubeClientAgentImage, "kube-client-agent manifest image")
	fs.StringVar(&r.setupEtcdEnvImage, "manifest-setup-etcd-env-image", r.setupEtcdEnvImage, "setup-etcd-env manifest image")
	fs.StringVar(&r.etcdDiscoveryDomain, "etcd-discovery-domain", r.etcdDiscoveryDomain, "etcd discovery domain")
	fs.StringVar(&r.etcdStaticResourcesDir, "etcd-static-resources-dir", r.etcdStaticResourcesDir, "path to etcd static resources directory")
	fs.StringVar(&r.etcdConfigDir, "etcd-config-dir", r.etcdConfigDir, "path to etcd config directory")
}

// Validate verifies the inputs.
func (r *renderOpts) Validate() error {
	if err := r.manifest.Validate(); err != nil {
		return err
	}
	if err := r.generic.Validate(); err != nil {
		return err
	}
	if len(r.etcdCAFile) == 0 {
		return errors.New("missing required flag: --etcd-ca")
	}
	if len(r.etcdMetricCAFile) == 0 {
		return errors.New("missing required flag: --etcd-metric-ca")
	}
	if len(r.etcdImage) == 0 {
		return errors.New("missing required flag: --manifest-etcd-image")
	}
	if len(r.kubeClientAgentImage) == 0 {
		return errors.New("missing required flag: --manifest-kube-client-agent-image")
	}
	if len(r.setupEtcdEnvImage) == 0 {
		return errors.New("missing required flag: --manifest-setup-etcd-env-image")
	}
	if len(r.clusterEtcdOperatorImage) == 0 {
		return errors.New("missing required flag: --manifest-cluster-etcd-operator-image")
	}
	if len(r.etcdDiscoveryDomain) == 0 {
		return errors.New("missing required flag: --etcd-discovery-domain")
	}
	return nil
}

// Complete fills in missing values before command execution.
func (r *renderOpts) Complete() error {
	if err := r.manifest.Complete(); err != nil {
		return err
	}
	if err := r.generic.Complete(); err != nil {
		return err
	}
	return nil
}

type TemplateData struct {
	options.ManifestConfig
	options.FileConfig

	// EtcdDiscoveryDomain is the domain used for SRV discovery.
	EtcdDiscoveryDomain    string
	EtcdServerCertDNSNames string
	EtcdPeerCertDNSNames   string
}

type StaticFile struct {
	name           string
	source         string
	destinationDir string
	mode           os.FileMode
	Data           []byte
}

// Run contains the logic of the render command.
func (r *renderOpts) Run() error {
	renderConfig := &TemplateData{
		ManifestConfig: options.ManifestConfig{
			Images: options.Images{
				Etcd:            r.etcdImage,
				SetupEtcdEnv:    r.setupEtcdEnvImage,
				KubeClientAgent: r.kubeClientAgentImage,
			},
		},
		EtcdDiscoveryDomain: r.etcdDiscoveryDomain,
		EtcdServerCertDNSNames: strings.Join([]string{
			"localhost",
			"etcd.kube-system.svc",
			"etcd.kube-system.svc.cluster.local",
			"etcd.openshift-etcd.svc",
			"etcd.openshift-etcd.svc.cluster.local",
		}, ","),
		EtcdPeerCertDNSNames: strings.Join([]string{
			r.etcdDiscoveryDomain,
		}, ","),
	}

	staticFiles := []StaticFile{
		{
			"ca.crt",
			r.etcdCAFile,
			r.etcdStaticResourcesDir,
			0644,
			nil,
		},
		{
			"metric-ca.crt",
			r.etcdMetricCAFile,
			r.etcdStaticResourcesDir,
			0644,
			nil,
		},
	}

	etcdConfPath := filepath.Join(r.generic.TemplatesDir, "config", "etc-etcd-etcd-conf.yaml")
	etcdConf, err := parseStaticTemplateFile(etcdConfPath)
	if err != nil {
		return err
	}
	staticFiles = append(staticFiles, StaticFile{filepath.Base(etcdConf.Path), "", r.etcdConfigDir, os.FileMode(*etcdConf.Mode), []byte(etcdConf.Contents.Inline)})
	files, err := populateFileData(staticFiles)
	if err != nil {
		return err
	}
	if err := writeStaticFiles(files); err != nil {
		return err
	}

	if err := r.manifest.ApplyTo(&renderConfig.ManifestConfig); err != nil {
		return err
	}
	if err := r.generic.ApplyTo(
		&renderConfig.FileConfig,
		options.Template{FileName: "defaultconfig.yaml", Content: v430_00_assets.MustAsset(filepath.Join(bootstrapVersion, "etcd", "defaultconfig.yaml"))},
		mustReadTemplateFile(filepath.Join(r.generic.TemplatesDir, "config", "bootstrap-config-overrides.yaml")),
		mustReadTemplateFile(filepath.Join(r.generic.TemplatesDir, "config", "config-overrides.yaml")),
		&renderConfig,
		nil,
	); err != nil {
		return err
	}

	return WriteFiles(&r.generic, &renderConfig.FileConfig, renderConfig)
}

// parseStaticTemplateFile takes a path to a yaml template file returns a populated File struct.
func parseStaticTemplateFile(path string) (*File, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	file := new(File)
	if err := yaml.Unmarshal(data, file); err != nil {
		return nil, fmt.Errorf("failed to unmarshal file into struct: %v", err)
	}
	return file, nil
}

// populateFileData takes a slice of StaticFile and populates Data from StaticFile.source.
// If source is not populated, skip.
func populateFileData(files []StaticFile) ([]StaticFile, error) {
	for i, file := range files {
		if file.source == "" {
			continue
		}
		data, err := ioutil.ReadFile(file.source)
		if err != nil {
			return nil, err
		}
		files[i].Data = data
	}
	return files, nil
}

func writeStaticFiles(files []StaticFile) error {
	for _, m := range files {
		if len(m.Data) == 0 {
			return fmt.Errorf("file %s has no data:", m.name)
		}
		b := m.Data
		path := filepath.Join(m.destinationDir, m.name)
		dirname := filepath.Dir(path)
		if err := os.MkdirAll(dirname, 0755); err != nil {
			return err
		}
		if err := ioutil.WriteFile(path, b, m.mode); err != nil {
			return err
		}
	}
	return nil
}

func mustReadTemplateFile(fname string) options.Template {
	bs, err := ioutil.ReadFile(fname)
	if err != nil {
		panic(fmt.Sprintf("Failed to load %q: %v", fname, err))
	}
	return options.Template{FileName: fname, Content: bs}
}

// WriteFiles writes the manifests and the bootstrap config file.
func WriteFiles(opt *options.GenericOptions, fileConfig *options.FileConfig, templateData interface{}, additionalPredicates ...assets.FileInfoPredicate) error {
	// write assets
	for _, manifestDir := range []string{"bootstrap-manifests", "manifests"} {
		manifests, err := assets.New(filepath.Join(opt.TemplatesDir, manifestDir), templateData, append(additionalPredicates, assets.OnlyYaml)...)
		if err != nil {
			return fmt.Errorf("failed rendering assets: %v", err)
		}
		if err := manifests.WriteFiles(filepath.Join(opt.AssetOutputDir, manifestDir)); err != nil {
			return fmt.Errorf("failed writing assets to %q: %v", filepath.Join(opt.AssetOutputDir, manifestDir), err)
		}
	}

	// create bootstrap configuration
	if err := ioutil.WriteFile(opt.ConfigOutputFile, fileConfig.BootstrapConfig, 0644); err != nil {
		return fmt.Errorf("failed to write merged config to %q: %v", opt.ConfigOutputFile, err)
	}

	return nil
}
