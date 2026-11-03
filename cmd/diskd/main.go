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

// stopTimeout is how long the daemon may take to finish what it is doing
// when it is asked to stop. A copy that is cut in the middle leaves the
// source where it was, but the daemon should not be killed while it is still
// writing.
const stopTimeout = 2 * time.Minute

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

	// The daemon lives until the signal: the context it is started with is
	// its lifetime, not a start timeout.
	if err := daemon.Start(ctx); err != nil {
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
