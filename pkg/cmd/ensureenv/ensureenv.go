package ensureenv

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"
)

// Current bash behavior (what this command is going to replace: fail if NODE_IP is empty):
// ensure-env
//		--ip-ev=NODE_IP
//		--allow-invalid-ip-ev-val=false
//		--check-set-and-not-empty=NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST,NODE_NODE_ENVVAR_NAME_ETCD_NAME,NODE_NODE_ENVVAR_NAME_IP
//		--ip-ev-equals=NODE_NODE_ENVVAR_NAME_IP
//		--ip-ev-equals=NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST

// Intermediate behavior (until NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST can be deprecated and removed)
// ensure-env
//		--ip-ev=NODE_IP
//		--allow-invalid-ip-ev-val=true
//		--fallback-ip-ev=NODE_NODE_ENVVAR_NAME_IP
//		--check-set-and-not-empty=NODE_NODE_ENVVAR_NAME_ETCD_NAME,NODE_NODE_ENVVAR_NAME_IP,NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST
//		--ip-ev-equals=NODE_NODE_ENVVAR_NAME_IP
//		--ip-ev-equals=NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST

// New behavior (try to get an IP address from network interfaces):
// ensure-env
//		--ip-ev=NODE_IP
//		--allow-invalid-ip-ev-val=true
//		--fallback-ip-ev=NODE_NODE_ENVVAR_NAME_IP
//		--check-set-and-not-empty=NODE_NODE_ENVVAR_NAME_ETCD_NAME,NODE_NODE_ENVVAR_NAME_IP
//		--ip-ev-equals=NODE_NODE_ENVVAR_NAME_IP

type ensureOpts struct {
	errOut           io.Writer
	allowInvalidIPEV bool
	ipEV             string
	fallbackIPEV     string
	notEmpty         []string
	shouldEqual      []string
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
	fs.BoolVar(&e.allowInvalidIPEV, "allow-invalid-ip-ev-val", true, "If this is set and the value of ip-ev is invalid (not equal to environment variables given by --ip-ev-equals), continue validation and check the value of --fallback-ip-ev against the network interfaces.")
	fs.StringVar(&e.ipEV, "ip-ev", "NODE_IP", "The environment variable that should contain the node IP address.")
	fs.StringVar(&e.fallbackIPEV, "fallback-ip-ev", "NODE_NODE_ENVVAR_NAME_IP", "If the value of the environment variable given by --ip-ev is not set (or is empty) and --allow-invalid-ip-ev-val is true, use this environment variable to check against the network interfaces to find the nodes ip address.")
	fs.StringSliceVar(&e.notEmpty, "check-set-and-not-empty", []string{}, "Validate that the given environment variable(s) are set and have non-empty values.")
	fs.StringSliceVar(&e.shouldEqual, "ip-ev-equals", []string{}, "Validate that the value of the given environment variables are equal.")
}

func (e *ensureOpts) Validate() (err error) {
	// Validate that the incoming environment variables are set and have non-empty values.
	for _, ev := range e.notEmpty {
		if value, has := os.LookupEnv(ev); !has {
			return fmt.Errorf("The value of environment variable %v must be set.", ev)
		} else if value == "" {
			return fmt.Errorf("The value of environment variable %v must not be empty.", ev)
		}
	}

	// Validate that the given environment variables are equal; this does not distinguish if the values are empty.
	for _, ev := range e.shouldEqual {
		var (
			ipEV = os.Getenv(e.ipEV)
			v    = os.Getenv(ev)
		)
		// Check both the raw value and the value as an array (wrapped in []).
		//		If they aren't equal and we accept an invalid ip-ev value, don't error and continue on.
		if !e.allowInvalidIPEV && (v != ipEV && v != fmt.Sprintf("[%s]", ipEV)) {
			return fmt.Errorf("The value of environment variables %v, %v must be equal. Were: \"%v\", \"%v\"", e.ipEV, ev, ipEV, v)
		}
	}

	// If --allow-invalid-ip-ev-val is true, --fallback-ip-ev MUST be set and have a non-empty value.
	if e.allowInvalidIPEV {
		if e.fallbackIPEV == "" {
			return fmt.Errorf("Since --allow-invalid-ip-ev-val is true, --fallback-ip-ev must be provided.")
		} else if os.Getenv(e.fallbackIPEV) == "" {
			return fmt.Errorf("Since --allow-invalid-ip-ev-val is true, the value of the environment variable provided by --fallback-ip-ev [%v] must not be empty.", e.fallbackIPEV)
		}
	}

	return
}

func (e *ensureOpts) Run() (err error) {
	var (
		fallbackEV = os.Getenv(e.fallbackIPEV)
		ipEV       = os.Getenv(e.ipEV)
	)

	// If we have a value for ip-ev and it must be equal to
	if ipEV == "" {
		var (
			interfaces []net.Interface
		)
		// Since the value of ip-ev is empty, we're going to check our
		//   fallback environment variable against our network
		//   interfaces, to see if we find a match.
		// If we find a match, we'll consider ourselves valid, if we can't
		//   we'll fail.
		if interfaces, err = net.Interfaces(); err != nil {
			return
		}

		for _, iface := range interfaces {
			var (
				addrs []net.Addr
			)

			if addrs, err = iface.Addrs(); err != nil {
				return
			}

			for _, addr := range addrs {
				if strings.Contains(addr.String(), fallbackEV) {
					// We found a match, we'll assume now that the value of fallbackEV is good
					//		and we'll continue allowing etcd to start.
					return nil
				}
			}
		}

		// If we drop out of the loop, we haven't found a match and we need to error-out
		return fmt.Errorf("Failed to find any ip addresses in network interfaces that match the value of %v", e.fallbackIPEV)
	}

	return
}
