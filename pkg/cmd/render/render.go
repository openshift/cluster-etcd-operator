package render

import (
	"fmt"
	"io"
	"io/ioutil"
	"path/filepath"

	"github.com/ghodss/yaml"
	"github.com/golang/glog"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	kubecontrolplanev1 "github.com/openshift/api/kubecontrolplane/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/v311_00_assets"
	genericrender "github.com/openshift/library-go/pkg/operator/render"
	genericrenderoptions "github.com/openshift/library-go/pkg/operator/render/options"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	bootstrapVersion = "v3.11.0"
)

// renderOpts holds values to drive the render command.
type renderOpts struct {
	manifest genericrenderoptions.ManifestOptions
	generic  genericrenderoptions.GenericOptions

	clusterConfigFile string
	disablePhase2     bool
	errOut            io.Writer
}

// NewRenderCommand creates a render command.
func NewRenderCommand(errOut io.Writer) *cobra.Command {
	renderOpts := &renderOpts{
		manifest: *genericrenderoptions.NewManifestOptions("etcd", "openshift/origin-hyperkube:latest"),
		generic:  *genericrenderoptions.NewGenericOptions(),
		errOut:   errOut,
	}
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render kubernetes controller manager bootstrap manifests, secrets and configMaps",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						glog.Fatal(err)
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
	r.manifest.AddFlags(fs, "controller manager")
	r.generic.AddFlags(fs, kubecontrolplanev1.GroupVersion.WithKind("KubeControllerManagerConfig"))

	fs.StringVar(&r.clusterConfigFile, "cluster-config-file", r.clusterConfigFile, "Openshift Cluster API Config file.")

	// TODO: remove when the installer has stopped using it
	fs.BoolVar(&r.disablePhase2, "disable-phase-2", r.disablePhase2, "Disable rendering of the phase 2 daemonset and dependencies.")
	fs.MarkHidden("disable-phase-2")
	fs.MarkDeprecated("disable-phase-2", "Only used temporarily to synchronize roll out of the phase 2 removal. Does nothing anymore.")
}

// Validate verifies the inputs.
func (r *renderOpts) Validate() error {
	if err := r.manifest.Validate(); err != nil {
		return err
	}
	if err := r.generic.Validate(); err != nil {
		return err
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
	genericrenderoptions.TemplateData

	ClusterCIDR           []string
	ServiceClusterIPRange []string
}

func discoverRestrictedCIDRs(clusterConfigFileData []byte, renderConfig *TemplateData) error {
	configJson, err := yaml.YAMLToJSON(clusterConfigFileData)
	if err != nil {
		return err
	}

	clusterConfigObj, err := runtime.Decode(unstructured.UnstructuredJSONScheme, configJson)
	if err != nil {
		return err
	}
	clusterConfig, ok := clusterConfigObj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected object in %t", clusterConfigObj)
	}

	if clusterCIDR, found, err := unstructured.NestedStringSlice(
		clusterConfig.Object, "spec", "clusterNetwork", "pods", "cidrBlocks"); found && err == nil {
		renderConfig.ClusterCIDR = clusterCIDR
	}
	if err != nil {
		return err
	}
	if serviceClusterIPRange, found, err := unstructured.NestedStringSlice(
		clusterConfig.Object, "spec", "clusterNetwork", "services", "cidrBlocks"); found && err == nil {
		renderConfig.ServiceClusterIPRange = serviceClusterIPRange
	}
	if err != nil {
		return err
	}
	return nil
}

// Run contains the logic of the render command.
func (r *renderOpts) Run() error {
	renderConfig := TemplateData{}
	if len(r.clusterConfigFile) > 0 {
		clusterConfigFileData, err := ioutil.ReadFile(r.clusterConfigFile)
		if err != nil {
			return err
		}
		err = discoverRestrictedCIDRs(clusterConfigFileData, &renderConfig)
		if err != nil {
			return fmt.Errorf("unable to parse restricted CIDRs from config: %v", err)
		}
	}
	if err := r.manifest.ApplyTo(&renderConfig.ManifestConfig); err != nil {
		return err
	}
	if err := r.generic.ApplyTo(
		&renderConfig.FileConfig,
		genericrenderoptions.Template{FileName: "defaultconfig.yaml", Content: v311_00_assets.MustAsset(filepath.Join(bootstrapVersion, "etcd", "defaultconfig.yaml"))},
		mustReadTemplateFile(filepath.Join(r.generic.TemplatesDir, "config", "bootstrap-config-overrides.yaml")),
		mustReadTemplateFile(filepath.Join(r.generic.TemplatesDir, "config", "config-overrides.yaml")),
		&renderConfig,
	); err != nil {
		return err
	}

	// add additional kubeconfig asset
	if kubeConfig, err := r.readBootstrapSecretsKubeconfig(); err != nil {
		return fmt.Errorf("failed to read %s/kubeconfig: %v", r.manifest.SecretsHostPath, err)
	} else {
		renderConfig.Assets["kubeconfig"] = kubeConfig
	}

	return genericrender.WriteFiles(&r.generic, &renderConfig.FileConfig, renderConfig)
}

func (r *renderOpts) readBootstrapSecretsKubeconfig() ([]byte, error) {
	return ioutil.ReadFile(filepath.Join(r.generic.AssetInputDir, "..", "auth", "kubeconfig"))
}

func mustReadTemplateFile(fname string) genericrenderoptions.Template {
	bs, err := ioutil.ReadFile(fname)
	if err != nil {
		panic(fmt.Sprintf("Failed to load %q: %v", fname, err))
	}
	return genericrenderoptions.Template{FileName: fname, Content: bs}
}
