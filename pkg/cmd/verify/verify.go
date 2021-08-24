package verify

import (
	"io"

	"github.com/spf13/cobra"
)

type verifyStorageOpts struct {
	errOut     io.Writer
	kubeconfig string
}

// NewVerifyCommand checks to verify preconditions and exit 0 on success.
func NewVerifyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "performs checks to verify preconditions and exit 0 on success",
		Run:   func(cmd *cobra.Command, args []string) {},
	}
	cmd.AddCommand(NewVerifyBackupStorage())
	return cmd
}
