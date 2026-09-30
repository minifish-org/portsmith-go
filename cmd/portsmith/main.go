// Command portsmith is the standalone entry point for the Portsmith migration
// workbench. It connects process signal cancellation to the library context and
// exits with the status returned by portsmith.Main.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/minifish-org/portsmith-go/internal/portsmith"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(portsmith.Main(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
