package mcp

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	core "github.com/hoangndst/buddy-agent/internal/mcp"
	"github.com/spf13/cobra"
)

// AddOptions specifies parameters for adding an MCP server.
type AddOptions struct {
	IsGlobal  bool
	RemoteURL string
	Headers   []string
}

// NewCmdAdd constructs the 'buddy mcp add' subcommand.
func NewCmdAdd(f *cmdutil.Factory) *cobra.Command {
	opts := &AddOptions{}

	cmd := &cobra.Command{
		Use:   "add [name] [flags] [-- <command> [args...]]",
		Short: "Add a local stdio or remote HTTP/SSE MCP server",
		Example: `  buddy mcp add sqlite -- uvx mcp-server-sqlite --db-path ./test.db
  buddy mcp add --url https://api.example.com/mcp --header "Authorization: Bearer $TOKEN" remote-docs
  buddy mcp add my-tool --global -- ./bin/my-tool`,
		RunE: func(c *cobra.Command, args []string) error {
			return runAdd(c, f, opts, args)
		},
	}

	cmd.Flags().BoolVar(&opts.IsGlobal, "global", false, "Save to global config (~/.buddy/mcp.json)")
	cmd.Flags().StringVar(&opts.RemoteURL, "url", "", "Endpoint URL for remote HTTP/SSE servers")
	cmd.Flags().StringArrayVar(&opts.Headers, "header", nil, "Custom HTTP headers for remote servers (repeatable)")

	return cmd
}

func runAdd(c *cobra.Command, f *cmdutil.Factory, opts *AddOptions, args []string) error {
	dashIdx := c.ArgsLenAtDash()
	if dashIdx != -1 {
		// Stdio server: name is before '--', command follows '--'
		preArgs := args[:dashIdx]
		cmdArgs := args[dashIdx:]

		if len(preArgs) == 0 {
			return errors.New("missing server name. Syntax: buddy mcp add <name> [--global] -- <command> [args...]")
		}
		serverName := preArgs[0]
		if len(cmdArgs) == 0 {
			return errors.New("command following '--' cannot be empty")
		}

		targetPath, err := getTargetConfigPath(f, opts.IsGlobal)
		if err != nil {
			return err
		}

		serverCfg := core.ServerConfig{
			Command: cmdArgs[0],
			Args:    cmdArgs[1:],
		}

		if err := core.AddOrUpdateServerEntry(targetPath, serverName, serverCfg); err != nil {
			return fmt.Errorf("failed to save server %q: %w", serverName, err)
		}

		scope := "project"
		if opts.IsGlobal {
			scope = "global"
		}
		_, _ = fmt.Fprintf(f.IOStreams.Out, "✓ Added MCP stdio server %q (%s config: %s)\n", serverName, scope, targetPath)
		return nil
	}

	// Remote SSE server
	if len(args) == 0 {
		return errors.New("missing server name. Syntax: buddy mcp add --url <url> [--header 'Key: Value'] <name>")
	}
	serverName := args[0]
	if opts.RemoteURL == "" {
		return errors.New("either '--' followed by a command or '--url <url>' must be specified")
	}

	headerMap := make(map[string]string)
	for _, h := range opts.Headers {
		parts := strings.SplitN(h, ":", 2)
		if len(parts) == 2 {
			headerMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}

	targetPath, err := getTargetConfigPath(f, opts.IsGlobal)
	if err != nil {
		return err
	}

	serverCfg := core.ServerConfig{
		URL:     opts.RemoteURL,
		Headers: headerMap,
	}

	if err := core.AddOrUpdateServerEntry(targetPath, serverName, serverCfg); err != nil {
		return fmt.Errorf("failed to save server %q: %w", serverName, err)
	}

	scope := "project"
	if opts.IsGlobal {
		scope = "global"
	}
	_, _ = fmt.Fprintf(f.IOStreams.Out, "✓ Added MCP remote SSE server %q (%s config: %s)\n", serverName, scope, targetPath)
	return nil
}
