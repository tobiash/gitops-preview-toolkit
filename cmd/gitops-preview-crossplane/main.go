package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/zerologr"
	"github.com/rs/zerolog"

	"github.com/tobiash/gitops-preview-toolkit/pkg/crossplanerender"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

var errUsage = errors.New("usage: gitops-preview-crossplane --socket PATH")

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run() error {
	socket := flag.String("socket", "", "Unix socket on which to serve the Crossplane plugin (required)")
	flag.Parse()
	if *socket == "" || flag.NArg() != 0 {
		return errUsage
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()
	service := crossplanerender.New(zerologr.New(&logger))
	err := plugin.Serve(ctx, *socket, service)
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return errors.Join(err, service.Close())
}
