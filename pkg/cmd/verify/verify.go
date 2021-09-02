package verify

import (
	"io"

	"github.com/spf13/cobra"
)

type verifyStorageOpts struct {
	errOut io.Writer
}

// NewVerifyCommand checks to verify preconditions and exit 0 on success.
func NewVerifyCommand(errOut io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "performs checks to verify preconditions and exit 0 on success",
		Run:   func(cmd *cobra.Command, args []string) {},
	}
	cmd.AddCommand(NewVerifyBackupStorage(errOut))
	return cmd
}
