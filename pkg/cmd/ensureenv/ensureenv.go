package ensureenv

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type ensureOpts struct {
	nodeIP       string
	notEmpty     []string
	equalsNodeIP []string
}

func NewEnsureEnvCommand() *cobra.Command {
	ensure := &ensureOpts{}

	cmd := &cobra.Command{
		Use:          "ensure-env",
		Short:        "Ensures that the IP address related environment variables are correct",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ensure.run()
		},
	}

	ensure.addFlags(cmd.Flags())
	return cmd
}

func (e *ensureOpts) addFlags(fs *pflag.FlagSet) {
	fs.StringVar(&e.nodeIP, "node-ip", "", "The internal IP of the node provided by the downward API.")
	fs.StringSliceVar(&e.notEmpty, "check-set-and-not-empty", []string{}, "Validate that the given environment variable(s) are set and have non-empty values.")
	fs.StringSliceVar(&e.equalsNodeIP, "check-equals-node-ip", []string{}, "Validate that the given environment variable(s) are equal to --node-ip.")
}

func (e *ensureOpts) run() error {
	if e.nodeIP == "" {
		return fmt.Errorf("--node-ip must be set to a non-empty value")
	}

	normalizedNodeIP := stripBrackets(e.nodeIP)

	for _, ev := range e.notEmpty {
		value, has := os.LookupEnv(ev)
		if !has {
			return fmt.Errorf("environment variable %s must be set", ev)
		}
		if value == "" {
			return fmt.Errorf("environment variable %s must not be empty", ev)
		}
	}

	for _, ev := range e.equalsNodeIP {
		value, has := os.LookupEnv(ev)
		if !has {
			return fmt.Errorf("environment variable %s must be set", ev)
		}
		if stripBrackets(value) != normalizedNodeIP {
			return fmt.Errorf("expected %s to be %s, got %s", ev, e.nodeIP, value)
		}
	}

	return nil
}

func stripBrackets(ip string) string {
	if strings.HasPrefix(ip, "[") && strings.HasSuffix(ip, "]") {
		return ip[1 : len(ip)-1]
	}
	return ip
}
