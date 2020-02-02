package membership

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/pkg/transport"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"

	"github.com/openshift/cluster-etcd-operator/pkg/cmd/waitforkube"
)

const (
	retryDuration     = 2 * time.Second
	dialTimeout       = 5 * time.Second
	etcdCertFile      = "/var/run/secrets/etcd-client/tls.crt"
	etcdKeyFile       = "/var/run/secrets/etcd-client/tls.key"
	etcdTrustedCAFile = "/var/run/configmaps/etcd-ca/ca-bundle.crt"
	dbPath            = "member/snap/db"
)

type membership struct {
	errOut        io.Writer
	EtcdDnsName   string
	EtcdEndpoints string
	etcdDataDir   string
}

// NewWaitForKubeCommand waits for kube to come up before continuing to start the rest of the containers.
func NewMembershipCommand(errOut io.Writer) *cobra.Command {
	membership := &membership{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "membership",
		Short: "cluster membership validate",
		Long:  "This command makes sure that the local etcd member is part of the cluster.",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
						fmt.Fprint(membership.errOut, err.Error())
					}
				}
			}
			must(membership.Complete)
			must(membership.Validate)
			must(membership.Run)
		},
	}
	membership.AddFlags(cmd.Flags())

	return cmd
}

func (m *membership) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&m.EtcdDnsName, "etcd-dns-name", "", "etcd member FQDN")
	fs.StringVar(&m.EtcdEndpoints, "endpoints", "", "commas seperated list of etcd clients")
	fs.StringVar(&m.etcdDataDir, "data-dir", "/var/lib/etcd", "path to etcd data-dir")
}
func (m *membership) Validate() error {
	if waitforkube.CheckEtcdDataFileExists(filepath.Join(m.etcdDataDir, dbPath)) {
		if m.EtcdEndpoints == "" || m.EtcdDnsName == "" {
			return fmt.Errorf("membership validation requires etcd-dns-name and endpoints")
		}
	}
	return nil
}

func (m *membership) Complete() error {
	etcdDnsName, etcdEndpoints := os.Getenv("ETCD_DNS_NAME"), os.Getenv("ETCD_ENDPOINTS")

	// flags take priority over ENV if used.
	if m.EtcdDnsName != "" && etcdDnsName != "" {
		klog.Infof("flag --etcd-dns-name and ENV ETCD_DNS_NAME set, ENV will be ignored")
	}
	if m.EtcdEndpoints != "" && etcdEndpoints != "" {
		klog.Infof("flag --endpoints and ENV ETCD_ENDPOINTS set, ENV will be ignored")
	}

	if m.EtcdEndpoints == "" && etcdEndpoints != "" {
		klog.Infof("recognized and used environment variable ETCD_ENDPOINTS=%s", etcdEndpoints)
		m.EtcdEndpoints = etcdEndpoints
	}
	if m.EtcdDnsName == "" && etcdDnsName != "" {
		klog.Infof("recognized and used environment variable ETCD_DNS_NAME=%s", etcdDnsName)
		m.EtcdDnsName = etcdDnsName
	}
	return nil
}

func (m *membership) Run() error {
	if waitforkube.CheckEtcdDataFileExists(filepath.Join(m.etcdDataDir, dbPath)) {
		klog.Infof("etcd has previously been initialized as a member")
		return nil
	}

	cli, err := getEtcdClient(strings.Split(m.EtcdEndpoints, ","))
	if err != nil {
		return err
	}
	defer cli.Close()

	wait.PollInfinite(retryDuration, func() (bool, error) {
		ctx, cancel := context.WithCancel(context.Background())
		l, err := cli.MemberList(ctx)
		cancel()
		if err != nil {
			klog.Errorf("memberlist failed: %v", err)
			return false, nil
		}

		//TODO we should handle IP based peers as well
		for _, member := range l.Members {
			if member.Name == m.EtcdDnsName {
				klog.Infof("%s found in cluster", m.EtcdDnsName)
				return true, nil
			}
		}
		klog.Infof("%s is currently not a member of the cluster %+v", m.EtcdDnsName, l.Members)
		return false, nil
	})

	return nil
}

func getEtcdClient(endpoints []string) (*clientv3.Client, error) {
	tlsInfo := transport.TLSInfo{
		CertFile:      etcdCertFile,
		KeyFile:       etcdKeyFile,
		TrustedCAFile: etcdTrustedCAFile,
	}
	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg := &clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: dialTimeout,
		TLS:         tlsConfig,
	}

	cli, err := clientv3.New(*cfg)
	if err != nil {
		return nil, err
	}
	return cli, nil
}
