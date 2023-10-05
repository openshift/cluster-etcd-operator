package hypershift

import (
	"os"

	"github.com/openshift/cluster-etcd-operator/pkg/cmd/hypershift/defragoperator"
	"github.com/spf13/cobra"
)

func NewHypershiftCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hypershift",
		Short: "Hypershift specific etcd commands",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
			os.Exit(1)
		},
	}

	cmd.AddCommand(defragoperator.NewDefragOperatorCommand())

	return cmd
}
