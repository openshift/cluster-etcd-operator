package operator

import (
	"context"
	"fmt"
	"net/http"

	"github.com/openshift/cluster-etcd-operator/pkg/operator"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/spf13/cobra"
	"k8s.io/apiserver/pkg/server/healthz"
)

func NewOperator() *cobra.Command {
	cmd := controllercmd.
		NewControllerCommandConfig("openshift-cluster-etcd-operator", version.Get(), operator.RunOperator).
		WithHealthChecks(healthz.NamedCheck("controller-aliveness", func(_ *http.Request) error {
			if !operator.AlivenessChecker.Alive() {
				return fmt.Errorf("found unhealthy aliveness check, returning error")
			}
			return nil
		})).
		NewCommandWithContext(context.Background())
	cmd.Use = "operator"
	cmd.Short = "Start the Cluster etcd Operator"

	return cmd
}
