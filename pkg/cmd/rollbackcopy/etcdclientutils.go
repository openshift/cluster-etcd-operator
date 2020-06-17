package rollbackcopy

import (
	"context"
	"encoding/json"
	"fmt"
	"google.golang.org/grpc"
	"io"
	"k8s.io/klog"
	"os"
	"time"

	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/pkg/transport"
)

func getEtcdClient(endpoints []string) (*clientv3.Client, error) {
	dialOptions := []grpc.DialOption{
		grpc.WithBlock(), // block until the underlying connection is up
	}

	tlsInfo := transport.TLSInfo{
		CertFile:      os.Getenv("ETCDCTL_CERT"),
		KeyFile:       os.Getenv("ETCDCTL_KEY"),
		TrustedCAFile: os.Getenv("ETCDCTL_CACERT"),
	}
	tlsConfig, err := tlsInfo.ClientConfig()

	cfg := &clientv3.Config{
		DialOptions: dialOptions,
		Endpoints:   endpoints,
		DialTimeout: 2 * time.Second,
		TLS:         tlsConfig,
	}

	cli, err := clientv3.New(*cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to make etcd client for endpoints %v: %w", endpoints, err)
	}
	return cli, nil
}

func getClusterVersionAndUpgradeInfo(cli *clientv3.Client) (string, bool, error) {
	upgradeInProgress := false
	clusterversionKey := "/kubernetes.io/config.openshift.io/clusterversions/version"
	resp, err := cli.Get(context.Background(), clusterversionKey)
	if err != nil {
		return "", false, err
	}
	if len(resp.Kvs) != 1 {
		return "", false, fmt.Errorf("Expected to get a single key from etcd, got %d", len(resp.Kvs))
	}

	var cv map[string]interface{}
	if err := json.Unmarshal(resp.Kvs[0].Value, &cv); err != nil {
		return "", false, err
	}

	status := cv["status"].(map[string]interface{})
	desired := status["desired"].(map[string]interface{})

	history := status["history"].([]interface{})
	if len(history) <= 0 {
		return "", false, fmt.Errorf("getClusterVersionAndUpgradeInfo: status has no history")
	}
	latestHistory := history[0].(map[string]interface{})
	latestVersion := latestHistory["version"].(string)
	if latestVersion == "" {
		upgradeInProgress = true
	} else if latestHistory["state"].(string) != "Completed" {
		upgradeInProgress = true
	} else if desired["version"].(string) != latestVersion {
		upgradeInProgress = true
	}
	return latestVersion, upgradeInProgress, nil
}

func checkLeadership(name string) (bool, error) {
	cli, err := getEtcdClient([]string{"localhost:2379"})
	if err != nil {
		return false, fmt.Errorf("checkLeadership: failed to get etcd client: %w", err)
	}
	defer cli.Close()

	membersResp, err := cli.MemberList(context.Background())
	if err != nil {
		return false, err
	}
	for _, member := range membersResp.Members {
		if member.Name != name {
			continue
		}
		if len(member.ClientURLs) == 0 && member.Name == "" {
			return false, fmt.Errorf("EtcdMemberNotStarted")
		}

		resp, err := cli.Status(context.Background(), member.ClientURLs[0])
		if err != nil {
			return false, fmt.Errorf("isLeader: error getting the status of the member %s. err=%w", member.Name, err)
		}
		return resp.Header.MemberId == resp.Leader, nil
	}
	return false, fmt.Errorf("EtcdMemberStatusUnknown")
}

func saveSnapshot(cli *clientv3.Client, dbPath string) error {
	partpath := dbPath + ".part"
	defer os.RemoveAll(partpath)

	f, err := os.OpenFile(partpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("could not open %s (%w)", partpath, err)
	}

	opBegin := time.Now()
	var rd io.ReadCloser
	rd, err = cli.Snapshot(context.Background())
	if err != nil {
		return fmt.Errorf("saveSnapshot failed: %w", err)
	}

	if _, err := io.Copy(f, rd); err != nil {
		return fmt.Errorf("saveSnapshot failed: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("saveSnapshot failed: %w", err)
	}
	klog.Info(
		"fetched snapshot. ",
		"Time taken: ", time.Since(opBegin),
	)

	if err := os.Rename(partpath, dbPath); err != nil {
		return fmt.Errorf("could not rename %s to %s (%v)", partpath, dbPath, err)
	}
	klog.Info("saved snapshot to path", dbPath)
	return nil
}
