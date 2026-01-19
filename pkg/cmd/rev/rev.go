package rev

import (
	"context"
	"encoding/json"
	"errors"
	goflag "flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/spf13/cobra"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/klog/v2"
)

type revOpts struct {
	clientCertFile   string
	clientKeyFile    string
	clientCACertFile string
	endpoints        []string
	pollInterval     time.Duration
	outputFile       string

	clientPool *etcdcli.EtcdClientPool
}

type outputStruct struct {
	ClusterId           uint64            `json:"clusterId,omitempty"`
	RaftIndexByEndpoint map[string]uint64 `json:"raftIndex,omitempty"`
	MaxRaftIndex        uint64            `json:"maxRaftIndex,omitempty"`
	CreatedTime         string            `json:"created,omitempty"`
}

func newRevOpts() *revOpts {
	return &revOpts{}
}

func NewRevCommand(ctx context.Context) *cobra.Command {
	opts := newRevOpts()
	cmd := &cobra.Command{
		Use:   "rev",
		Short: "Starts a continuous revision monitor, which saves them locally at an interval.",
		Run: func(cmd *cobra.Command, args []string) {
			defer klog.Flush()

			if err := opts.Validate(); err != nil {
				klog.Fatal(err)
			}
			if err := opts.Run(ctx); err != nil {
				klog.Fatal(err)
			}
		},
	}

	opts.AddFlags(cmd)
	return cmd
}

func (r *revOpts) AddFlags(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.StringVar(&r.clientCertFile, "client-cert-file", r.clientCertFile, "Etcd TLS client certificate file. (required)")
	fs.StringVar(&r.clientKeyFile, "client-key-file", r.clientKeyFile, "Etcd TLS client key file. (required)")
	fs.StringVar(&r.clientCACertFile, "client-cacert-file", r.clientCACertFile, "Etcd TLS client CA certificate file. (required)")
	fs.StringSliceVar(&r.endpoints, "endpoints", r.endpoints, "Comma separated list of peer endpoints to connect to (required)")
	fs.DurationVar(&r.pollInterval, "poll-interval", 10*time.Second, "Poll interval, by default every 10s")
	fs.StringVar(&r.outputFile, "output-file", "/var/lib/etcd/revision.json", "File path to output to, by default /var/lib/etcd/revision.json")

	// adding klog flags to tune verbosity better
	gfs := goflag.NewFlagSet("", goflag.ExitOnError)
	klog.InitFlags(gfs)
	cmd.Flags().AddGoFlagSet(gfs)
}

func (r *revOpts) Validate() error {
	if len(r.clientCertFile) == 0 {
		return errors.New("missing required flag: --client-cert-file")
	}
	if len(r.clientKeyFile) == 0 {
		return errors.New("missing required flag: --client-key-file")
	}
	if len(r.clientCACertFile) == 0 {
		return errors.New("missing required flag: --client-cacert-file")
	}
	if len(r.endpoints) == 0 {
		return errors.New("missing required flag: --endpoints")
	}

	return nil
}

// Run will run a ticker to continuously poll endpoints and write the revisions to the defined file.
// The resulting file behavior during failures is:
//   - A file is only ever written when there is at least one endpoint responding
//   - The maximum revision is always the max from all endpoint results in a ticker invocation,
//     no historic values from previous files are taken into account - TODO(thomas): we might need to reconsider this
//   - The file, once written entirely, is atomically renamed to its destination path. This ensures
//     we don't ever have half-written and torn json files.
//   - During quorum split scenarios, the node(s) on the non-quorum side will not have have the latest
//     revisions anymore. Here we trade-off the availability of any number with overall correctness.
//     We have to account for this during the restore procedure by bumping revisions with some slack.
func (r *revOpts) Run(ctx context.Context) error {
	tlsInfo := transport.TLSInfo{
		CertFile:      r.clientCertFile,
		KeyFile:       r.clientKeyFile,
		TrustedCAFile: r.clientCACertFile,
	}

	clientPool := etcdcli.NewEtcdClientPool(
		// newFunc
		func() (*clientv3.Client, error) { return newETCD3Client(ctx, r.endpoints, tlsInfo) },
		// endpointsFunc
		func() ([]string, error) { return r.endpoints, nil },
		// healthFunc
		func(client *clientv3.Client) error { return nil },
		// closeFunc
		func(client *clientv3.Client) error { return client.Close() },
	)

	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case _ = <-ticker.C:
			trySaveRevision(ctx, r.endpoints, r.outputFile, clientPool, r.pollInterval)
		}
	}
}

func trySaveRevision(ctx context.Context, endpoints []string, outputFile string, pool *etcdcli.EtcdClientPool, timeout time.Duration) {
	wg := sync.WaitGroup{}
	lock := sync.Mutex{}

	wg.Add(len(endpoints))
	clusterId := uint64(0)
	raftIndexByEndpoint := map[string]uint64{}
	maxRaftIndex := uint64(0)
	for _, ep := range endpoints {
		go func(ep string) {
			defer wg.Done()

			timeoutCtx, cancelFunc := context.WithTimeout(ctx, timeout)
			defer cancelFunc()

			client, err := pool.Get()
			if err != nil {
				klog.Errorf("error creating client: %v", err)
				return
			}
			defer pool.Return(client)

			status, err := client.Status(timeoutCtx, ep)
			if err != nil {
				klog.Errorf("error getting status from [%s]: %v", ep, err)
				return
			}

			lock.Lock()
			defer lock.Unlock()

			raftIndexByEndpoint[ep] = status.RaftIndex
			maxRaftIndex = max(maxRaftIndex, status.RaftIndex)
			clusterId = status.Header.ClusterId
		}(ep)
	}

	wg.Wait()

	if len(raftIndexByEndpoint) == 0 {
		klog.Errorf("could not reach any endpoint to get the raft index")
		return
	}

	jsonOutput, err := json.Marshal(outputStruct{
		ClusterId:           clusterId,
		RaftIndexByEndpoint: raftIndexByEndpoint,
		MaxRaftIndex:        maxRaftIndex,
		CreatedTime:         time.Now().UTC().String(),
	})
	if err != nil {
		klog.Errorf("error marshalling json: %v", err)
		return
	}

	tmpPath := fmt.Sprintf("%s.tmp", outputFile)
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		klog.Errorf("error opening file: %v", err)
		return
	}
	defer func() {
		if err = file.Close(); err != nil {
			klog.Errorf("error closing file: %v", err)
		}
	}()

	_, err = file.Write(jsonOutput)
	if err != nil {
		klog.Errorf("error writing result to file: %v", err)
		return
	}

	if err = os.Rename(tmpPath, outputFile); err != nil {
		klog.Errorf("error during rename to destination file: %v", err)
		return
	}
}

func newETCD3Client(ctx context.Context, endpoints []string, tlsInfo transport.TLSInfo) (*clientv3.Client, error) {
	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg := &clientv3.Config{
		Endpoints: endpoints,
		TLS:       tlsConfig,
		Context:   ctx,
	}

	return clientv3.New(*cfg)
}
