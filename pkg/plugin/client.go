package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Options configures transport bounds. Zero durations select defaults; negative
// durations are invalid. Caller deadlines are always respected when shorter.
type Options struct {
	StartupTimeout  time.Duration // Default 10 seconds.
	RPCTimeout      time.Duration // Default 2 minutes, including render calls.
	ShutdownTimeout time.Duration // Default 10 seconds before SIGKILL, allowing runtime cleanup.
}

func (o Options) defaults() (Options, error) {
	if o.StartupTimeout < 0 || o.RPCTimeout < 0 || o.ShutdownTimeout < 0 {
		return o, errors.New("plugin timeouts must not be negative")
	}
	if o.StartupTimeout == 0 {
		o.StartupTimeout = 10 * time.Second
	}
	if o.RPCTimeout == 0 {
		o.RPCTimeout = 2 * time.Minute
	}
	if o.ShutdownTimeout == 0 {
		o.ShutdownTimeout = 10 * time.Second
	}
	return o, nil
}

// Client owns a persistent connection and its executable. Close is idempotent
// and must be called by the owner; cancellation of Start's context also closes
// the client. Failed/uncertain RPCs invalidate the complete engine process.
type Client struct {
	conn      *grpc.ClientConn
	process   *exec.Cmd
	options   Options
	command   Command
	dir       string
	stderr    *boundedStderr
	done      chan struct{}
	exitErr   error // Published by closing done.
	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error // Published by closing closed.
}

var _ Service = (*Client)(nil)

// Start launches a trusted executable, appending --socket and a private socket
// path to its arguments, and verifies Describe before returning it.
func Start(ctx context.Context, command Command) (*Client, error) {
	return StartWithOptions(ctx, command, Options{})
}

// StartWithOptions is Start with configurable transport timeouts.
func StartWithOptions(ctx context.Context, command Command, options Options) (*Client, error) {
	options, err := options.defaults()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if command.Command == "" || command.Name == "" {
		return nil, errors.New("plugin command and expected name are required")
	}
	command.Config = append([]byte(nil), command.Config...)
	// A short path also avoids Unix socket path length limits when TMPDIR is
	// a long workspace path. MkdirTemp creates the directory with mode 0700.
	dir, err := os.MkdirTemp("/tmp", "fmp-")
	if err != nil {
		return nil, fmt.Errorf("create private plugin directory: %w", err)
	}
	socket := filepath.Join(dir, "rpc.sock")
	args := append(append([]string(nil), command.Args...), "--socket", socket)
	cmd := exec.Command(command.Command, args...)
	configureProcess(cmd)
	stderr := new(boundedStderr)
	cmd.Stderr = stderr
	// Bound os/exec's pipe-copy wait even when a descendant inherits stderr.
	cmd.WaitDelay = options.ShutdownTimeout
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("start plugin %q: %w", command.Name, err)
	}
	c := &Client{process: cmd, options: options, command: command, dir: dir, stderr: stderr, done: make(chan struct{}), closed: make(chan struct{})}
	go func() { c.exitErr = cmd.Wait(); close(c.done) }()
	conn, err := grpc.NewClient("passthrough:///"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}),
		grpc.WithDisableRetry(), grpc.WithDisableServiceConfig(),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: backoff.Config{BaseDelay: 10 * time.Millisecond, Multiplier: 1.5, Jitter: 0.2, MaxDelay: 100 * time.Millisecond}, MinConnectTimeout: time.Second}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMessageBytes), grpc.MaxCallSendMsgSize(MaxMessageBytes)),
	)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("connect plugin %q: %w", command.Name, err)
	}
	c.conn = conn
	startup, cancel := context.WithTimeout(ctx, options.StartupTimeout)
	defer cancel()
	// Process exit interrupts readiness immediately rather than waiting the
	// full startup timeout. WaitForReady waits for a connection, not RPC retry.
	monitorDone := make(chan struct{})
	go func() {
		select {
		case <-c.done:
			cancel()
		case <-startup.Done():
		}
		close(monitorDone)
	}()
	request, err := encode(&DescribeRequest{ProtocolVersion: ProtocolVersion})
	var response DescribeResponse
	if err == nil {
		message := new(wrapperspb.BytesValue)
		err = conn.Invoke(startup, "/"+serviceName+"/Describe", request, message, grpc.WaitForReady(true))
		if err == nil {
			err = decode(message, &response)
		}
	}
	cancel()
	<-monitorDone
	if err == nil && (response.ProtocolVersion != ProtocolVersion || response.Name != command.Name) {
		err = fmt.Errorf("handshake mismatch: got name %q protocol %d, expected name %q protocol %d", response.Name, response.ProtocolVersion, command.Name, ProtocolVersion)
	}
	if err != nil {
		switch status.Code(err) {
		case codes.DeadlineExceeded:
			err = errors.Join(err, context.DeadlineExceeded)
		case codes.Canceled:
			err = errors.Join(err, context.Canceled)
		}
		_ = c.Close()
		return nil, c.failure("startup", err)
	}
	// Install only after construction so cancellation cannot race conn setup.
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-c.done:
			_ = c.Close()
		case <-c.closed:
		}
	}()
	if err := ctx.Err(); err != nil {
		_ = c.Close()
		return nil, c.failure("startup", err)
	}
	return c, nil
}

func (c *Client) invoke(ctx context.Context, method string, request, response any) error {
	select {
	case <-c.closed:
		return c.failure(method, errors.New("plugin client is closed"))
	default:
	}
	select {
	case <-c.done:
		_ = c.Close()
		return c.failure(method, errors.New("plugin process exited"))
	default:
	}
	if err := ctx.Err(); err != nil {
		return c.failure(method, err)
	}
	message, err := encode(request)
	if err != nil {
		return err
	} // Nothing dispatched; engine state is certain.
	ctx, cancel := context.WithTimeout(ctx, c.options.RPCTimeout)
	defer cancel()
	output := new(wrapperspb.BytesValue)
	err = c.conn.Invoke(ctx, "/"+serviceName+"/"+method, message, output)
	if err == nil {
		err = decode(output, response)
		if err != nil {
			_ = c.Close()
		}
	} else {
		switch status.Code(err) {
		case codes.DeadlineExceeded:
			err = errors.Join(err, context.DeadlineExceeded)
		case codes.Canceled:
			err = errors.Join(err, context.Canceled)
		}
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		switch status.Code(err) {
		case codes.Canceled, codes.DeadlineExceeded, codes.Unavailable, codes.Unknown, codes.Internal, codes.ResourceExhausted:
			// A response may have been lost after a session mutation. Never
			// reuse or automatically replay against this uncertain engine.
			_ = c.Close()
		}
	}
	if err != nil {
		return c.failure(method, err)
	}
	return nil
}

func (c *Client) Describe(ctx context.Context, request *DescribeRequest) (*DescribeResponse, error) {
	response := new(DescribeResponse)
	if err := c.invoke(ctx, "Describe", request, response); err != nil {
		return nil, err
	}
	return response, nil
}

func (c *Client) OpenRender(ctx context.Context, request *OpenRequest) (*OpenResponse, error) {
	if request == nil {
		return nil, errors.New("nil OpenRender request")
	}
	copy := *request
	if len(copy.Config) == 0 {
		copy.Config = c.command.Config
	}
	response := new(OpenResponse)
	if err := c.invoke(ctx, "OpenRender", &copy, response); err != nil {
		return nil, err
	}
	return response, nil
}

func (c *Client) Expand(ctx context.Context, request *ExpandRequest) (*ExpandResponse, error) {
	response := new(ExpandResponse)
	if err := c.invoke(ctx, "Expand", request, response); err != nil {
		return nil, err
	}
	return response, nil
}

func (c *Client) CloseRender(ctx context.Context, request *CloseRequest) (*CloseResponse, error) {
	response := new(CloseResponse)
	if err := c.invoke(ctx, "CloseRender", request, response); err != nil {
		return nil, err
	}
	return response, nil
}

// Close disconnects, signals the whole child process group, then kills it after
// the shutdown bound. It also kills surviving descendants after the leader exits.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		// Do not wait for the context callback: it may itself be calling Close.
		if c.conn != nil {
			c.closeErr = c.conn.Close()
		}
		_ = signalProcessGroup(c.process, false)
		timer := time.NewTimer(c.options.ShutdownTimeout)
		select {
		case <-c.done:
		case <-timer.C:
		}
		timer.Stop()
		_ = signalProcessGroup(c.process, true)
		timer = time.NewTimer(c.options.ShutdownTimeout)
		select {
		case <-c.done:
		case <-timer.C:
			c.closeErr = errors.Join(c.closeErr, errors.New("plugin process did not exit after SIGKILL"))
		}
		timer.Stop()
		c.closeErr = errors.Join(c.closeErr, os.RemoveAll(c.dir))
		close(c.closed)
	})
	return c.closeErr
}

func (c *Client) failure(operation string, err error) error {
	var exit string
	select {
	case <-c.done:
		if c.exitErr != nil {
			exit = "; process: " + c.exitErr.Error()
		}
	default:
	}
	return fmt.Errorf("plugin %q %s: %w%s; stderr (last 16 KiB): %s", c.command.Name, operation, err, exit, c.stderr.String())
}

const stderrLimit = 16 << 10

// Capture only engine-written stderr; never log request/config/resource data.
type boundedStderr struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedStderr) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(p) >= stderrLimit {
		b.data = append(b.data[:0], p[len(p)-stderrLimit:]...)
	} else {
		if excess := len(b.data) + len(p) - stderrLimit; excess > 0 {
			b.data = b.data[excess:]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

func (b *boundedStderr) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }
