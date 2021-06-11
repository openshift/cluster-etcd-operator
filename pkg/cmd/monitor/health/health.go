package health

import (
	"context"
	"fmt"
	"time"

	"go.etcd.io/etcd/clientv3"
	"go.uber.org/zap"
	"google.golang.org/grpc/connectivity"
)

const (
	SerializedReadSingleTarget CheckName = "SerializedReadSingleTarget"
	QuorumReadSingleTarget     CheckName = "QuorumReadSingleTarget"
	GRPCReadySingleTarget      CheckName = "GRPCReadySingleTarget"
	QuorumRead                 CheckName = "QuorumRead"

	DefaultSlowRequestDuration = 500 * time.Millisecond
	DefaultContextDeadline     = 1 * time.Second
	DefaultNamespaceKey        = "/kubernetes.io/namespaces/default"
)

type CheckFunc func(context.Context, *Check)

type CheckName string

type CheckTarget string

type CheckStatus struct {
	logDisruptionStart time.Time
	logDisruptionEnd   time.Time
	available          bool
	disruption         bool
	slowRequest        bool
	check              CheckName
}

type Check struct {
	ctx     context.Context
	client  *clientv3.Client
	lg      *zap.Logger
	name    CheckName
	targets []string
	status  CheckStatus
	Probe   CheckFunc
}

func NewCheck(lg *zap.Logger, client *clientv3.Client, targets []string) *Check {
	return &Check{
		client:  client,
		lg:      lg,
		targets: targets,
	}
}

// Health checks will log requests over DefaultSlowRequestDuration with a timeout at DefaultContextDeadline.

// WithSerializedReadSingleTarget performs a serialized RangeRequest against a single target. This test is of lower
// overall importance because linearized read/write are the default for kubernetes. But some requests will not fail
// if quorum is not established. Examples include WatchRequest that does no pass WithRequireLeader, DefragmentRequest,
// HashKVRequest and MemberList (3.4 and earlier).
func WithSerializedReadSingleTarget() CheckFunc {
	return func(ctx context.Context, c *Check) {
		c.name = SerializedReadSingleTarget
		c.checkKVSRead(ctx, []clientv3.OpOption{clientv3.WithSerializable(), clientv3.WithKeysOnly()})
	}
}

// WithQuorumReadSingleTarget performs a quorate RangeRequest against a single target. This check is useful to
// understand exact timings on quorate availability on a per peer level. This single target test can fail on a
// minority of peer targets while WithQuorumRead passes. This can happen most commonly on node reboot during
// upgrades or during a static-pod rollout. But can also happen less during a network partition.
func WithQuorumReadSingleTarget() CheckFunc {
	return func(ctx context.Context, c *Check) {
		c.name = QuorumReadSingleTarget
		c.checkKVSRead(ctx, []clientv3.OpOption{clientv3.WithKeysOnly()})
	}
}

// WithGRPCReadySingleTarget performs a gRPC dial to the endpoint and checks if the service is "Ready". This check
// ensures that the target subconn will be considered usable by the etcd client balancer for round robin requests. Ready
// does not preclude quorum.
func WithGRPCReadySingleTarget() CheckFunc {
	return func(ctx context.Context, c *Check) {
		c.name = GRPCReadySingleTarget
		c.checkDialStatus(ctx)
	}
}

// WithQuorumRead performs a quorate RangeRequest against all targets. This check ensures given an etcd client
// with multiple endpoint targets the client balancer can provide linearized read/write service to the apiserver.
func WithQuorumRead() CheckFunc {
	return func(ctx context.Context, c *Check) {
		c.name = QuorumRead
		c.checkKVSRead(ctx, []clientv3.OpOption{clientv3.WithKeysOnly()})
	}
}

func (c *Check) checkDialStatus(_ context.Context) {
	c.ensureCheckStatus()
	// Due to client retry failure will not be observed directly.
	slowRequestTimer := time.AfterFunc(DefaultSlowRequestDuration, func() {
		c.logSlowRequest(fmt.Errorf("slow request"))
	})
	defer slowRequestTimer.Stop()

	resp, err := c.client.Dial(c.targets[0])
	if err != nil {
		c.logHealthFail(err)
		return
	}
	defer resp.Close()
	if resp.GetState() == connectivity.Ready {
		c.logHealthSuccess()
		return
	}
	c.logHealthFail(fmt.Errorf("grpc: ClientConn state %q", resp.GetState()))
}

func (c *Check) checkKVSRead(ctx context.Context, opts []clientv3.OpOption) {
	c.ensureCheckStatus()

	//Due to client retry failure will not be observed directly.
	slowRequestTimer := time.AfterFunc(DefaultSlowRequestDuration, func() {
		c.logSlowRequest(fmt.Errorf("slow request"))
	})
	defer slowRequestTimer.Stop()

	// Without a context timeout we will retry non mutating request indefinitely.
	ctx, cancel := context.WithTimeout(ctx, DefaultContextDeadline)
	defer cancel()
	resp, err := c.client.Get(ctx, DefaultNamespaceKey, opts...)
	if err != nil {
		c.logHealthFail(err)
		return
	}
	if resp != nil && len(resp.Kvs) == 0 {
		c.logHealthFail(fmt.Errorf("default namespace%s not found", DefaultNamespaceKey))
		return
	}
	c.logHealthSuccess()
}

func (c *Check) ensureCheckStatus() {
	// start counter before call to capture timeout duration, toggle slow request signal.
	if !c.status.disruption {
		c.status.slowRequest = true
		c.status.logDisruptionStart = time.Now()
	}
	return
}
