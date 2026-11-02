// Command diskd moves files between the configured folders so that the
// source volumes keep their free space.
//
// Nothing is moved without an approval in the web interface.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/app"
)

const (
	startTimeout = 30 * time.Second
	stopTimeout  = 2 * time.Minute
)

// The build information, filled in by the linker.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	configPath := flag.String("config", "", "path to the configuration file")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "error: --config is required")
		os.Exit(2)
	}

	daemon, err := app.New(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("diskd %s (%s, %s)\n", version, commit, date)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCtx, cancelStart := context.WithTimeout(context.Background(), startTimeout)
	defer cancelStart()
	if err := daemon.Start(startCtx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	<-ctx.Done()

	stopCtx, cancelStop := context.WithTimeout(context.Background(), stopTimeout)
	defer cancelStop()
	if err := daemon.Stop(stopCtx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
