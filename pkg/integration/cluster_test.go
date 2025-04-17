package integration

import (
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/server/v3/etcdserver"
	"testing"
)

func TestErrorStringMatches(t *testing.T) {
	// Careful: this error string is referenced in many places in this codebase by string due to protobuf marshalling issues.
	// When this message changes, please do a code search and find and update all references
	require.Equal(t, etcdserver.ErrLearnerNotReady.Error(), "etcdserver: can only promote a learner member which is in sync with leader")
}
