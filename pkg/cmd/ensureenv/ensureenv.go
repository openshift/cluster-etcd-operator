package ensureenv

import (
	"fmt"
	"io"
	"net"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"
)

// Current bash behavior (what this command is going to replace: fail if NODE_IP is empty):
// ensure-env
//		--node-ip=$(NODE_IP)
//		--allow-invalid-node-ip=false
//		--check-set-and-not-empty=NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST,NODE_NODE_ENVVAR_NAME_ETCD_NAME,NODE_NODE_ENVVAR_NAME_IP
//		--current-revision-node-ip=$(NODE_NODE_ENVVAR_NAME_IP)

// New behavior (try to get an IP address from network interfaces):
// ensure-env
//		--node-ip=$(NODE_IP)
//		--allow-invalid-node-ip=true
//		--current-revision-node-ip=$(NODE_NODE_ENVVAR_NAME_IP)
//		--check-set-and-not-empty=NODE_NODE_ENVVAR_NAME_ETCD_NAME,NODE_NODE_ENVVAR_NAME_IP

type ensureOpts struct {
	errOut             io.Writer
	allowInvalidNodeIP bool
	nodeIP             string
	currentRevNodeIP   string
	notEmpty           []string
}

func NewEnsureEnvCommand(errOut io.Writer) *cobra.Command {
	ensure := &ensureOpts{
		errOut: errOut,
	}

	cmd := &cobra.Command{
		Use:   "ensure-env",
		Short: "Ensures that the IP address related environment variables are correct",
		Run: func(cmd *cobra.Command, args []string) {
			if err := ensure.Validate(); err != nil {
				klog.Error(err.Error())
				fmt.Fprint(ensure.errOut, err)
				return
			}
			if err := ensure.Run(); err != nil {
				klog.Error(err.Error())
				fmt.Fprint(ensure.errOut, err)
				return
			}
		},
	}

	ensure.AddFlags(cmd.Flags())
	return cmd
}

func (e *ensureOpts) AddFlags(fs *pflag.FlagSet) {
	fs.BoolVar(&e.allowInvalidNodeIP, "allow-invalid-node-ip", true, "If this is set and the value of --node-ip is invalid (not equal to --current-revision-node-ip), continue validation and check the value of --current-revision-node-ip against the network interfaces.")
	fs.StringVar(&e.nodeIP, "node-ip", "", "The internal IP of the node provided by the downward API; can be empty in edge cases where the host took too long to respond and the downward API returned an empty value.")
	fs.StringVar(&e.currentRevNodeIP, "current-revision-node-ip", "", "The IP address we're expecting; validated against the downward API or the network interfaces.")
	fs.StringSliceVar(&e.notEmpty, "check-set-and-not-empty", []string{}, "Validate that the given environment variable(s) are set and have non-empty values.")
}

func (e *ensureOpts) Validate() (err error) {
	// Validate that the incoming environment variables are set and have non-empty values.
	for _, ev := range e.notEmpty {
		if value, has := os.LookupEnv(ev); !has {
			return fmt.Errorf("the value of environment variable %v must be set.", ev)
		} else if value == "" {
			return fmt.Errorf("the value of environment variable %v must not be empty.", ev)
		}
	}

	// Check node-ip must be set, unless we accept invalid (not set or empty) values.
	if e.nodeIP == "" && !e.allowInvalidNodeIP {
		return fmt.Errorf("since --allow-invalid-node-ip is not set, --node-ip must be set to a non-empty value.")
	}

	// Current revision must also be set.
	if e.currentRevNodeIP == "" {
		return fmt.Errorf("--current-revision-node-ip must be set to a non-empty value.")
	}

	return nil
}

func (e *ensureOpts) Run() (err error) {
	// If the node-ip isn't valid, we'll check the network interfaces.
	if e.nodeIP != e.currentRevNodeIP {
		if !e.allowInvalidNodeIP {
			return fmt.Errorf("since --allow-invalid-node-ip is not set, node-ip and current-revision-node-ip must be equal.")
		}

		// Since the value of nodeIP is invalid, we're going to check our
		//   fallback environment variable (current-revision-node-ip) against our network
		//   interfaces, to see if we find a match.
		// If we find a match, we'll consider ourselves valid, if we can't
		//   we'll fail.
		var interfaces []net.Interface
		if interfaces, err = net.Interfaces(); err != nil {
			return err
		}

		// Parse the current-revision-node-ip value into an IP structure to test for equality later.
		var currentRevIP = net.ParseIP(e.currentRevNodeIP)
		for _, iface := range interfaces {
			var addrs []net.Addr

			if addrs, err = iface.Addrs(); err != nil {
				return err
			}

			for _, addr := range addrs {
				switch ip := addr.(type) {
				// Casting here to remove the mask before checking for equality.
				case *net.IPNet:
					if ip.IP.Equal(currentRevIP) {
						// We found a match, we'll assume now that the value of fallbackEV is good
						// and we'll continue allowing etcd to start.
						klog.Info("NODE_IP is invalid, found a match to the current revision node IP in the network interfaces.")
						return nil
					}
				}
			}
		}

		// If we drop out of the loop, we haven't found a match and we need to error-out
		return fmt.Errorf("failed to find ip address on network interfaces: %v", e.currentRevNodeIP)
	}

	return nil
}
