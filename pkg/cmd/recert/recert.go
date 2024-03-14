package recert

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ghodss/yaml"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcdcertsigner"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"io"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
	"k8s.io/utils/path"
	"net"
	"os"
	"path/filepath"
)

// recertOpts holds values to drive the recert command.
type recertOpts struct {
	// Path to output certificates
	outputDir string
	errOut    io.Writer

	// hostname -> ip (v4/v6) as string
	hostIPs map[string]string
	force   bool
	// allowed values are json,yaml
	format string
}

// NewRecertCommand creates a recert command.
func NewRecertCommand(errOut io.Writer) *cobra.Command {
	renderOpts := recertOpts{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "recert",
		Short: "Recreates all etcd related certificates for the given IPs and hostnames.",
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
			must(renderOpts.Run)
		},
	}

	renderOpts.AddFlags(cmd.Flags())

	return cmd
}

func (r *recertOpts) AddFlags(fs *pflag.FlagSet) {
	fs.StringVarP(&r.outputDir, "output", "o", "",
		"output path for certificates, must be a non-existent directory that will be automatically created")
	fs.StringToStringVar(&r.hostIPs, "hips", map[string]string{},
		"a list of host=ip pairs, e.g. --hips \"master-1=192.168.2.1,master-2=192.168.2.2,master-3=192.168.2.3\"")
	fs.BoolVarP(&r.force, "force", "f", false, "skips hostname/IP validation")
	fs.StringVarP(&r.format, "format", "w", "yaml",
		"options yaml and json output secrets and configmaps. Example: -w json")
}

// Validate verifies the inputs.
func (r *recertOpts) Validate() error {
	if len(r.outputDir) == 0 {
		return errors.New("missing required flag: --output")
	}
	if len(r.hostIPs) == 0 {
		return errors.New("need at least one hostname/IP pair in: --hips")
	}

	if r.format != "yaml" && r.format != "json" {
		return fmt.Errorf("only supported formats are \"json\" and \"yaml\", supplied %s", r.format)
	}

	exists, err := path.Exists(path.CheckFollowSymlink, r.outputDir)
	if err != nil {
		return fmt.Errorf("error while checking whether output dir already exists: %w", err)
	}

	if exists {
		return fmt.Errorf("output dir %s already exists", r.outputDir)
	}

	if r.force {
		return nil
	}

	for hostName, ipAddress := range r.hostIPs {
		if ip := net.ParseIP(ipAddress); ip == nil {
			return fmt.Errorf("could not parse IP address: %s", ip)
		}
		if validationErrs := validation.IsDNS1123Label(hostName); validationErrs != nil {
			return fmt.Errorf("could not parse hostname as DNS1123 label: %s - %v", hostName, validationErrs)
		}
	}

	return nil
}

func (r *recertOpts) hostIPsAsNodes() []*corev1.Node {
	var nodes []*corev1.Node
	for hostName, ipAddress := range r.hostIPs {
		n := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: hostName, Labels: map[string]string{"node-role.kubernetes.io/master": ""}},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ipAddress}}},
		}
		nodes = append(nodes, n)
	}

	return nodes
}

func (r *recertOpts) Run() error {
	err := os.MkdirAll(r.outputDir, 0755)
	if err != nil {
		return fmt.Errorf("could not mkdir %s: %w", r.outputDir, err)
	}

	nodes := r.hostIPsAsNodes()
	certs, bundles, err := etcdcertsigner.CreateCertSecrets(nodes)
	if err != nil {
		return fmt.Errorf("could not create cert secrets, error was: %w", err)
	}

	for _, cert := range certs {
		destinationPath := filepath.Join(r.outputDir, fmt.Sprintf("%s-%s.%s", cert.Namespace, cert.Name, r.format))
		bytes, err := json.Marshal(cert)
		if err != nil {
			return fmt.Errorf("error while marshalling secret %s: %w", cert.Name, err)
		}
		if r.format == "yaml" {
			bytes, err = yaml.JSONToYAML(bytes)
			if err != nil {
				return fmt.Errorf("error while converting secret from json to yaml %s: %w", cert.Name, err)
			}
		}

		err = os.WriteFile(destinationPath, bytes, 0644)
		if err != nil {
			return fmt.Errorf("error while writing secret %s: %w", cert.Name, err)
		}
	}

	for _, bundle := range bundles {
		destinationPath := filepath.Join(r.outputDir, fmt.Sprintf("%s-%s.%s", bundle.Namespace, bundle.Name, r.format))
		bytes, err := json.Marshal(bundle)
		if err != nil {
			return fmt.Errorf("error while marshalling configmap %s: %w", bundle.Name, err)
		}
		if r.format == "yaml" {
			bytes, err = yaml.JSONToYAML(bytes)
			if err != nil {
				return fmt.Errorf("error while converting configmap from json to yaml %s: %w", bundle.Name, err)
			}
		}

		err = os.WriteFile(destinationPath, bytes, 0644)
		if err != nil {
			return fmt.Errorf("error while writing configmap %s: %w", bundle.Name, err)
		}
	}

	return nil
}
