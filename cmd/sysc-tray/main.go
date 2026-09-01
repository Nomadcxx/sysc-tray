// Command sysc-tray serves StatusNotifierItem and DBusMenu state to one shell.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Nomadcxx/sysc-tray/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := app.New(os.Getenv("XDG_RUNTIME_DIR")).Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "sysc-tray: %v\n", err)
		os.Exit(1)
	}
}
