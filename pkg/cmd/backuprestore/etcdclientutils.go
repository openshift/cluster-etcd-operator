package backuprestore

import (
	"context"
	"fmt"
	"google.golang.org/grpc"
	"io"
	"k8s.io/klog/v2"
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
