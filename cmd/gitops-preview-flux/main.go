package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/fluxrender"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (err error) {
	socket := flag.String("socket", "", "Unix socket for the persistent plugin service")
	flag.Parse()
	if *socket == "" {
		return fmt.Errorf("--socket is required")
	}
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc, err := fluxrender.New(logr.Discard())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, svc.Close()) }()
	err = plugin.Serve(ctx, *socket, svc)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
