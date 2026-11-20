// Package doc implements LLM-ready CLI documentation generation commands using cobra/doc.
package doc

import (
	"fmt"
	"os"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

// Options specifies documentation generation parameters.
type Options struct {
	Dir    string
	Stdout bool
}

// NewCmdDoc constructs the 'buddy doc' command.
func NewCmdDoc(f *cmdutil.Factory) *cobra.Command {
	opts := &Options{}

	cmd := &cobra.Command{
		Use:   "doc",
		Short: "Generate LLM-ready Markdown documentation for buddy-agent CLI commands",
		Long: `Generate comprehensive, structured Markdown documentation for all buddy-agent
commands, flags, and subcommands for developer references and LLM prompt context ingestion.`,
		Example: `  buddy doc --dir ./docs/cli
  buddy doc --stdout`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runDoc(c, f, opts)
		},
	}

	cmd.Flags().StringVarP(&opts.Dir, "dir", "d", "./docs/cli", "Directory to output Markdown documentation")
	cmd.Flags().BoolVar(&opts.Stdout, "stdout", false, "Write root command documentation to standard output")

	return cmd
}

func runDoc(c *cobra.Command, f *cmdutil.Factory, opts *Options) error {
	rootCmd := c.Root()

	if opts.Stdout {
		return doc.GenMarkdown(rootCmd, f.IOStreams.Out)
	}

	if err := os.MkdirAll(opts.Dir, 0755); err != nil {
		return fmt.Errorf("failed to create doc directory %q: %w", opts.Dir, err)
	}

	if err := doc.GenMarkdownTree(rootCmd, opts.Dir); err != nil {
		return fmt.Errorf("failed to generate markdown documentation tree: %w", err)
	}

	_, _ = fmt.Fprintf(f.IOStreams.Out, "✓ Generated LLM-ready CLI documentation in %s\n", opts.Dir)
	return nil
}
