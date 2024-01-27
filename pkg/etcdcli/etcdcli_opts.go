package etcdcli

import (
	"time"

	"go.etcd.io/etcd/client/pkg/v3/transport"
)

type ClientOptions struct {
	dialTimeout time.Duration
	tlsInfo     *transport.TLSInfo
}

func newClientOpts(opts ...ClientOption) (*ClientOptions, error) {
	clientOpts := &ClientOptions{
		dialTimeout: DefaultDialTimeout,
	}
	clientOpts.applyOpts(opts)
	return clientOpts, nil
}

func (co *ClientOptions) applyOpts(opts []ClientOption) {
	for _, opt := range opts {
		opt(co)
	}
}

type ClientOption func(*ClientOptions)

func WithDialTimeout(timeout time.Duration) ClientOption {
	return func(co *ClientOptions) {
		co.dialTimeout = timeout
	}
}

func WithTLSInfo(tlsInfo *transport.TLSInfo) ClientOption {
	return func(co *ClientOptions) {
		co.tlsInfo = tlsInfo
	}
}
