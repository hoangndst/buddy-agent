// Package cmdutil provides shared CLI infrastructure including IO streams and command factories.
package cmdutil

import (
	"bytes"
	"io"
	"os"
)

// IOStreams provides input/output abstractions for CLI commands, enabling
// testable execution without direct reliance on operating system file descriptors.
type IOStreams struct {
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
}

// SystemIOStreams returns standard operating system input/output streams.
func SystemIOStreams() *IOStreams {
	return &IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}
}

// TestIOStreams creates in-memory input/output buffers for deterministic testing.
func TestIOStreams() (*IOStreams, *bytes.Buffer, *bytes.Buffer, *bytes.Buffer) {
	in := &bytes.Buffer{}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	return &IOStreams{
		In:     in,
		Out:    out,
		ErrOut: errOut,
	}, in, out, errOut
}
