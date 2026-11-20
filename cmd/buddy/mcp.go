package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hoangndst/buddy-agent/internal/mcp"
	"github.com/spf13/cobra"
)

func newMCPCmd(ctx context.Context) *cobra.Command {
	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Manage Model Context Protocol (MCP) server configurations",
	}

	mcpCmd.AddCommand(newMCPListCmd(ctx))
	mcpCmd.AddCommand(newMCPAddCmd(ctx))
	mcpCmd.AddCommand(newMCPRemoveCmd(ctx))

	return mcpCmd
}

func newMCPListCmd(_ context.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all configured MCP servers and transports",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runMCPList()
		},
	}
}

func newMCPAddCmd(_ context.Context) *cobra.Command {
	var isGlobal bool
	var remoteURL string
	var headers []string

	cmd := &cobra.Command{
		Use:   "add [name] [flags] [-- <command> [args...]]",
		Short: "Add a local stdio or remote HTTP/SSE MCP server",
		Example: `  buddy mcp add sqlite -- uvx mcp-server-sqlite --db-path ./test.db
  buddy mcp add --url https://api.example.com/mcp --header "Authorization: Bearer $TOKEN" remote-docs
  buddy mcp add my-tool --global -- ./bin/my-tool`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dashIdx := cmd.ArgsLenAtDash()
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

				targetPath, err := getTargetConfigPath(isGlobal)
				if err != nil {
					return err
				}

				serverCfg := mcp.ServerConfig{
					Command: cmdArgs[0],
					Args:    cmdArgs[1:],
				}

				if err := mcp.AddOrUpdateServerEntry(targetPath, serverName, serverCfg); err != nil {
					return fmt.Errorf("failed to save server %q: %w", serverName, err)
				}

				scope := "project"
				if isGlobal {
					scope = "global"
				}
				cmd.Printf("✓ Added MCP stdio server %q (%s config: %s)\n", serverName, scope, targetPath)
				return nil
			}

			// Remote SSE server
			if len(args) == 0 {
				return errors.New("missing server name. Syntax: buddy mcp add --url <url> [--header 'Key: Value'] <name>")
			}
			serverName := args[0]
			if remoteURL == "" {
				return errors.New("either '--' followed by a command or '--url <url>' must be specified")
			}

			headerMap := make(map[string]string)
			for _, h := range headers {
				parts := strings.SplitN(h, ":", 2)
				if len(parts) == 2 {
					headerMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
				}
			}

			targetPath, err := getTargetConfigPath(isGlobal)
			if err != nil {
				return err
			}

			serverCfg := mcp.ServerConfig{
				URL:     remoteURL,
				Headers: headerMap,
			}

			if err := mcp.AddOrUpdateServerEntry(targetPath, serverName, serverCfg); err != nil {
				return fmt.Errorf("failed to save server %q: %w", serverName, err)
			}

			scope := "project"
			if isGlobal {
				scope = "global"
			}
			cmd.Printf("✓ Added MCP remote SSE server %q (%s config: %s)\n", serverName, scope, targetPath)
			return nil
		},
	}

	cmd.Flags().BoolVar(&isGlobal, "global", false, "Save to global config (~/.buddy/mcp.json)")
	cmd.Flags().StringVar(&remoteURL, "url", "", "Endpoint URL for remote HTTP/SSE servers")
	cmd.Flags().StringArrayVar(&headers, "header", nil, "Custom HTTP headers for remote servers (repeatable)")

	return cmd
}

func newMCPRemoveCmd(_ context.Context) *cobra.Command {
	var isGlobal bool

	cmd := &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Remove an MCP server from configuration",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverName := args[0]
			targetPath, err := getTargetConfigPath(isGlobal)
			if err != nil {
				return err
			}

			if err := mcp.RemoveServerEntry(targetPath, serverName); err != nil {
				if errors.Is(err, mcp.ErrServerNotFound) {
					scope := "project"
					if isGlobal {
						scope = "global"
					}
					return fmt.Errorf("server %q not found in %s configuration (%s)", serverName, scope, targetPath)
				}
				return fmt.Errorf("failed to remove server %q: %w", serverName, err)
			}

			scope := "project"
			if isGlobal {
				scope = "global"
			}
			cmd.Printf("✓ Removed MCP server %q from %s configuration (%s)\n", serverName, scope, targetPath)
			return nil
		},
	}

	cmd.Flags().BoolVar(&isGlobal, "global", false, "Remove from global config (~/.buddy/mcp.json)")
	return cmd
}

func getTargetConfigPath(global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to determine user home directory: %w", err)
		}
		return filepath.Join(home, ".buddy", "mcp.json"), nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to determine working directory: %w", err)
	}
	return filepath.Join(wd, ".buddy", "mcp.json"), nil
}

func runMCPList() error {
	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()

	cfg, err := mcp.LoadConfig(wd, home)
	if err != nil {
		return fmt.Errorf("failed to load mcp configuration: %w", err)
	}

	if len(cfg.MCPServers) == 0 {
		fmt.Println("No MCP servers configured.")
		fmt.Println("Add one using 'buddy mcp add <name> -- <command>' or 'buddy mcp add --url <url> <name>'.")
		return nil
	}

	fmt.Println("Configured Model Context Protocol (MCP) Servers:")
	fmt.Printf("%-20s %-10s %-10s %-10s %s\n", "NAME", "TRANSPORT", "SOURCE", "STATUS", "TARGET / COMMAND")
	fmt.Println(strings.Repeat("-", 80))

	for name, srv := range cfg.MCPServers {
		status := "enabled"
		if srv.Disabled {
			status = "disabled"
		}
		transport := string(srv.TransportType())
		target := srv.Command
		if len(srv.Args) > 0 {
			target += " " + strings.Join(srv.Args, " ")
		}
		if srv.URL != "" {
			target = srv.URL
		}
		fmt.Printf("%-20s %-10s %-10s %-10s %s\n", name, transport, srv.Source, status, target)
	}

	return nil
}
