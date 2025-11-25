package backuprestore

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"

	pb "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const (
	defaultTimeout = 2 * time.Second
	unixProtocol   = "unix"
)

// Code taken from github.com/kubernetes-sigs/cri-tools/cmd/crictl

var defaultRuntimeEndpoint string = "unix:///run/crio/crio.sock"

func getRuntimeCrioClient() (pb.RuntimeServiceClient, *grpc.ClientConn, error) {
	// Set up a connection to the server.
	conn, err := getConnection(defaultRuntimeEndpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("crio connect failed: %w", err)
	}
	runtimeClient := pb.NewRuntimeServiceClient(conn)
	return runtimeClient, conn, nil
}

func getConnection(endPoint string) (*grpc.ClientConn, error) {
	var conn *grpc.ClientConn
	klog.Infof("connect using endpoint '%s' with '%s' timeout", endPoint, defaultTimeout)
	addr, dialer, err := getAddressAndDialer(endPoint)
	if err != nil {
		return nil, err
	}
	conn, err = grpc.Dial(addr, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(defaultTimeout), grpc.WithContextDialer(dialer))
	if err != nil {
		errMsg := fmt.Errorf("connect endpoint '%s', make sure you are running as root and the endpoint has been started: %w", endPoint, err)
		return nil, errMsg
	}
	return conn, nil
}

// GetAddressAndDialer returns the address parsed from the given endpoint and a context dialer.
func getAddressAndDialer(endpoint string) (string, func(ctx context.Context, addr string) (net.Conn, error), error) {
	protocol, addr, err := parseEndpoint(endpoint)
	if err != nil {
		return "", nil, err
	}
	if protocol != unixProtocol {
		return "", nil, fmt.Errorf("only support unix socket endpoint")
	}

	return addr, dial, nil
}

func dial(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, unixProtocol, addr)
}

func parseEndpoint(endpoint string) (string, string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", err
	}

	switch u.Scheme {
	case "tcp":
		return "tcp", u.Host, nil

	case "unix":
		return "unix", u.Path, nil

	case "":
		return "", "", fmt.Errorf("using %q as endpoint is deprecated, please consider using full url format", endpoint)

	default:
		return u.Scheme, "", fmt.Errorf("protocol %q not supported", u.Scheme)
	}
}

// listContainers sends a ListContainerRequest to the server, and parses
// the returned ListContainerResponse.
func listContainers(ctx context.Context, runtimeClient pb.RuntimeServiceClient) ([]*pb.Container, error) {
	filter := &pb.ContainerFilter{}
	st := &pb.ContainerStateValue{}
	st.State = pb.ContainerState_CONTAINER_RUNNING
	request := &pb.ListContainersRequest{
		Filter: filter,
	}
	r, err := runtimeClient.ListContainers(ctx, request)
	if err != nil {
		return nil, err
	}
	return r.GetContainers(), nil
}
