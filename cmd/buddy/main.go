// Package main provides the command-line bootstrap entrypoint for buddy-agent.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hoangndst/buddy-agent/internal/cmd"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	factory := cmdutil.NewFactory()
	rootCmd := cmd.NewRootCmd(factory)

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		log.Fatal(err)
	}
}
