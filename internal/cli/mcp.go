package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/tobiash/gitops-preview-toolkit/pkg/agent"
	"github.com/tobiash/gitops-preview-toolkit/pkg/agentmcp"
)

func agentFlags(cmd *cobra.Command, root *string, opts *agent.Options) {
	cmd.Flags().StringVar(root, "root", "", "Workspace root (required)")
	cmd.Flags().BoolVar(&opts.Trusted, "trusted", false, "Enable trusted startup capabilities")
	cmd.Flags().DurationVar(&opts.TTL, "ttl", 0, "Handle TTL (0: service default)")
	cmd.Flags().IntVar(&opts.MaxSnapshots, "max-snapshots", 0, "Maximum retained handles (0: service default)")
	cmd.Flags().Int64Var(&opts.MaxBytes, "max-bytes", 0, "Maximum retained bytes (0: service default)")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 0, "Per-operation timeout (0: service default)")
	cmd.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		var err error
		cmd.InheritedFlags().VisitAll(func(f *pflag.Flag) {
			switch f.Name {
			case "crossplane", "crossplane-engine", "crossplane-function", "flux-plugin", "crossplane-plugin":
				return
			}
			if f.Changed {
				err = fmt.Errorf("legacy flag --%s is not supported by %s", f.Name, cmd.Name())
			}
		})
		if err != nil {
			return err
		}
		return configureAgentPlugins(opts)
	}
}

func configureAgentPlugins(opts *agent.Options) error {
	if crossplaneEnabled && !opts.Trusted {
		return fmt.Errorf("%w: --crossplane requires --trusted for agent/MCP startup", ErrUserInput)
	}
	// Startup flags supply all execution authority. Do not load repository runtime
	// settings here; agent operations apply repository data within their own policy.
	commands, err := runtimePluginCommands(nil, runtimeSettings{
		enabled: crossplaneEnabled, engine: crossplaneEngine, functions: crossplaneFunctions,
		flux: fluxPlugin, crossplane: crossplanePlugin,
	})
	if err != nil {
		return err
	}
	opts.PluginCommands = commands
	// Crossplane is represented in PluginCommands so executable selection and its
	// config stay together. The separate default-plugin toggle would duplicate it.
	opts.Crossplane = false
	opts.CrossplaneConfig = nil
	return nil
}

func mcpCmd() *cobra.Command {
	var root string
	var opts agent.Options
	cmd := &cobra.Command{Use: "mcp", Short: "Serve manifest tools over MCP stdio", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if root == "" {
				return fmt.Errorf("--root is required")
			}
			s, err := agent.New(root, opts)
			if err != nil {
				return err
			}
			defer func() {
				if err := s.Close(); err != nil {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), err)
				}
			}()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return agentmcp.NewServer(s, version).Run(ctx, agentmcp.StdioTransport())
		}}
	agentFlags(cmd, &root, &opts)
	return cmd
}
