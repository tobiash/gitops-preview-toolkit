package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
)

// Serve serves one persistent engine on a Unix socket until cancellation or a
// termination signal. Engines must honor RPC cancellation and support concurrent
// calls. Shutdown cancels outstanding calls before releasing engine resources.
// An engine implementing Close() error is closed exactly once, including on
// listener setup failure. Existing socket paths are never removed on startup.
func Serve(ctx context.Context, socket string, service Service) (result error) {
	if service == nil {
		return errors.New("plugin service is nil")
	}
	defer func() {
		if closer, ok := service.(interface{ Close() error }); ok {
			result = errors.Join(result, closer.Close())
		}
	}()
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	lifecycle, cancel := context.WithCancel(ctx)
	defer cancel()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen on plugin socket: %w", err)
	}
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, fmt.Errorf("close plugin listener: %w", err))
		}
	}()
	if err := os.Chmod(socket, 0o600); err != nil {
		return fmt.Errorf("secure plugin socket: %w", err)
	}
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(MaxMessageBytes), grpc.MaxSendMsgSize(MaxMessageBytes),
		grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			ctx, cancelRPC := context.WithCancel(ctx)
			defer cancelRPC()
			stop := context.AfterFunc(lifecycle, cancelRPC)
			defer stop()
			return handler(ctx, req)
		}),
	)
	server.RegisterService(&renderServiceDesc, service)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err = <-done:
	case <-lifecycle.Done():
	}
	cancel()
	graceful := make(chan struct{})
	go func() { server.GracefulStop(); close(graceful) }()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-graceful:
	case <-timer.C:
		server.Stop()
		// Stop closes transports even if an engine ignores cancellation. The
		// executable owner can then terminate the process within its bound.
	}
	if err != nil && !errors.Is(err, grpc.ErrServerStopped) && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("serve plugin: %w", err)
	}
	return nil
}
