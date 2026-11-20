package doc

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/spf13/cobra"
)

func TestDoc_TreeGeneration(t *testing.T) {
	tempDir := t.TempDir()
	outDir := filepath.Join(tempDir, "docs")

	f := cmdutil.NewFactory()
	ioStreams, _, outBuf, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	rootCmd := &cobra.Command{
		Use:   "buddy",
		Short: "buddy test root",
	}
	docCmd := NewCmdDoc(f)
	rootCmd.AddCommand(docCmd)

	rootCmd.SetArgs([]string{"doc", "--dir", outDir})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("doc command failed: %v", err)
	}

	if !strings.Contains(outBuf.String(), "Generated LLM-ready CLI documentation") {
		t.Errorf("expected success message, got: %s", outBuf.String())
	}
}

func TestDoc_Stdout(t *testing.T) {
	f := cmdutil.NewFactory()
	ioStreams, _, outBuf, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	rootCmd := &cobra.Command{
		Use:   "buddy",
		Short: "buddy test root",
	}
	docCmd := NewCmdDoc(f)
	rootCmd.AddCommand(docCmd)

	rootCmd.SetArgs([]string{"doc", "--stdout"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("doc --stdout failed: %v", err)
	}

	if !strings.Contains(outBuf.String(), "buddy") {
		t.Errorf("expected stdout to contain markdown documentation, got: %s", outBuf.String())
	}
}
