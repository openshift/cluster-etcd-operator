package e2e

import (
	"testing"

	scalingtestinglibrary "github.com/openshift/library-go/test/library/etcdverticalscaling"
)

// TestScalingUpAndDownSingleNode tests basic vertical scaling scenario.
// This scenario starts by adding a new master machine to the cluster
// next it validates the size of etcd cluster and makes sure the new member is healthy.
// The test ends by removing the newly added machine and validating the size of the cluster
// and asserting the member was removed from the etcd cluster by contacting MemberList API.
func TestScalingUpAndDownSingleNode(t *testing.T) {
	scalingtestinglibrary.TestScalingUpAndDownSingleNode(t)
}
