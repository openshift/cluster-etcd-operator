package setupetcd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/version"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/wait"
	klog "k8s.io/klog"
)

type setupOpts struct {
	discoverySRV string
	ifName       string
	outputFile   string
	errOut       io.Writer
}

// NewCertSignerCommand creates an etcd cert signer server.
func NewSetupEtcdCommand(errOut io.Writer) *cobra.Command {
	setupOpts := &setupOpts{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "setup-etcd-environment",
		Short: "sets up the environment for etcd",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(setupOpts.errOut, err.Error())
				}
			}
			must(setupOpts.Validate)
			must(setupOpts.Run)
		},
	}

	setupOpts.AddFlags(cmd.Flags())
	return cmd
}

func (s *setupOpts) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&s.discoverySRV, "discovery-srv", "", "DNS domain used to bootstrap initial etcd cluster.")
	fs.StringVar(&s.outputFile, "output-file", "", "file where the envs are written. If empty, prints to Stdout.")
	fs.Set("logtostderr", "true")
}

// Validate verifies the inputs.
func (s *setupOpts) Validate() error {
	if s.discoverySRV == "" {
		return errors.New("--discovery-srv cannot be empty")
	}
	return nil
}

func (s *setupOpts) Run() error {
	info := version.Get()

	// To help debugging, immediately log version
	klog.Infof("Version: %+v (%s)", info.GitVersion, info.GitCommit)

	ips, err := ipAddrs()
	if err != nil {
		return err
	}

	var dns string
	var ip string
	if err := wait.PollImmediate(30*time.Second, 5*time.Minute, func() (bool, error) {
		for _, cand := range ips {
			found, err := reverseLookupSelf("etcd-server-ssl", "tcp", s.discoverySRV, cand)
			if err != nil {
				klog.Errorf("error looking up self: %v", err)
				continue
			}
			if found != "" {
				dns = found
				ip = cand
				return true, nil
			}
			klog.V(4).Infof("no matching dns for %s", cand)
		}
		return false, nil
	}); err != nil {
		return fmt.Errorf("could not find self: %v", err)
	}
	klog.Infof("dns name is %s", dns)

	out := os.Stdout
	if s.outputFile != "" {
		f, err := os.Create(s.outputFile)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}

	return writeEnvironmentFile(map[string]string{
		"IPV4_ADDRESS":      ip,
		"DNS_NAME":          dns,
		"WILDCARD_DNS_NAME": fmt.Sprintf("*.%s", s.discoverySRV),
	}, out)
}

func ipAddrs() ([]string, error) {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips, err
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil {
			continue
		}
		ip = ip.To4()
		if ip == nil {
			continue // not an ipv4 address
		}
		if !ip.IsGlobalUnicast() {
			continue // we only want global unicast address
		}
		ips = append(ips, ip.String())
	}

	return ips, nil
}

// returns the target from the SRV record that resolves to self.
func reverseLookupSelf(service, proto, name, self string) (string, error) {
	_, srvs, err := net.LookupSRV(service, proto, name)
	if err != nil {
		return "", err
	}
	selfTarget := ""
	for _, srv := range srvs {
		klog.V(4).Infof("checking against %s", srv.Target)
		addrs, err := net.LookupHost(srv.Target)
		if err != nil {
			return "", fmt.Errorf("could not resolve member %q: %v", srv.Target, err)
		}

		for _, addr := range addrs {
			if addr == self {
				selfTarget = strings.Trim(srv.Target, ".")
				break
			}
		}
	}
	if selfTarget == "" {
		return "", fmt.Errorf("could not find self")
	}
	return selfTarget, nil
}

func writeEnvironmentFile(m map[string]string, w io.Writer) error {
	var buffer bytes.Buffer
	for k, v := range m {
		buffer.WriteString(fmt.Sprintf("ETCD_%s=%s\n", k, v))
	}
	if _, err := buffer.WriteTo(w); err != nil {
		return err
	}
	return nil
}
