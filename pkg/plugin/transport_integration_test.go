//go:build integration && (linux || darwin || freebsd || openbsd || netbsd || dragonfly)

package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// A real helper executable keeps state across RPCs, exercising the process,
// socket, framing, cancellation, session, and resource cleanup boundaries.
func TestPluginHelperProcess(t *testing.T) {
	args := os.Args
	marker := -1
	for i, arg := range args {
		if arg == "plugin-helper" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	mode, socket := args[marker+1], args[len(args)-1]
	if args[len(args)-2] != "--socket" {
		os.Exit(90)
	}
	if mode == "fail" {
		fmt.Fprint(os.Stderr, "startup exploded")
		os.Exit(23)
	}
	if mode == "notready" {
		select {}
	}
	if mode == "descendant" {
		signal.Ignore(os.Interrupt, syscall.SIGTERM)
		if err := os.WriteFile(socket, []byte("ready"), 0o600); err != nil {
			os.Exit(26)
		}
		select {}
	}
	s := &testEngine{mode: mode, sessions: make(map[string]int), socket: socket}
	if err := Serve(context.Background(), socket, s); err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(24)
	}
	os.Exit(0)
}

type testEngine struct {
	mu           sync.Mutex
	mode, socket string
	next         int
	sessions     map[string]int
}

func (s *testEngine) Describe(ctx context.Context, r *DescribeRequest) (*DescribeResponse, error) {
	version, name := ProtocolVersion, "helper"
	if s.mode == "version" {
		version++
	}
	if s.mode == "name" {
		name = "wrong"
	}
	return &DescribeResponse{ProtocolVersion: version, Name: name, Version: "test"}, nil
}

func (s *testEngine) OpenRender(ctx context.Context, r *OpenRequest) (*OpenResponse, error) {
	if s.mode == "tree" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		ready := s.socket + ".child"
		cmd := exec.Command(exe, "-test.run=^TestPluginHelperProcess$", "--", "plugin-helper", "descendant", "--socket", ready)
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		go func() { _ = cmd.Wait() }()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ticker.C:
			}
		}
		return &OpenResponse{Session: fmt.Sprint(cmd.Process.Pid)}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := fmt.Sprint(s.next)
	s.sessions[id] = 0
	return &OpenResponse{Session: id}, nil
}

func (s *testEngine) Expand(ctx context.Context, r *ExpandRequest) (*ExpandResponse, error) {
	if s.mode == "crash" {
		fmt.Fprint(os.Stderr, strings.Repeat("x", 40<<10)+"CRASH-END")
		os.Exit(25)
	}
	if s.mode == "slow" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.mode == "stubborn" {
		select {}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	count, ok := s.sessions[r.Session]
	if !ok {
		return nil, status.Error(codes.NotFound, "session not found")
	}
	// Cache the first materialization and reuse it across subsequent calls.
	if count == 0 {
		count = 1
		s.sessions[r.Session] = count
	}
	resources := r.Resources
	if s.mode == "huge-response" {
		resources = []Resource{{ID: "large", YAML: strings.Repeat("x", MaxMessageBytes)}}
	}
	return &ExpandResponse{Expansions: []Expansion{{ID: fmt.Sprintf("cached-%s-%d", r.Session, count), Trigger: "root", Resources: resources}}}, nil
}

func (s *testEngine) CloseRender(ctx context.Context, r *CloseRequest) (*CloseResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, r.Session)
	return &CloseResponse{}, nil
}

func (s *testEngine) Close() error { return os.WriteFile(s.socket+".closed", []byte("closed"), 0o600) }

func helperCommand(t *testing.T, mode string) Command {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Command{Name: "helper", Command: exe, Args: []string{"-test.run=^TestPluginHelperProcess$", "--", "plugin-helper", mode}}
}

func helperClient(t *testing.T, mode string, options Options) *Client {
	t.Helper()
	c, err := StartWithOptions(t.Context(), helperCommand(t, mode), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func TestPersistentSessions(t *testing.T) {
	c := helperClient(t, "normal", Options{})
	info, err := os.Stat(c.dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory: %v %v", info, err)
	}
	info, err = os.Stat(filepath.Join(c.dir, "rpc.sock"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private socket: %v %v", info, err)
	}
	a, err := c.OpenRender(t.Context(), &OpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.OpenRender(t.Context(), &OpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Session == b.Session {
		t.Fatal("sessions are not distinct")
	}
	resource := Resource{ID: "v1/ConfigMap/default/test", YAML: strings.Repeat("x", 5<<20), Provenance: Provenance{Path: "source.yaml"}}
	var cached string
	for range 2 {
		r, err := c.Expand(t.Context(), &ExpandRequest{Session: a.Session, Resources: []Resource{resource}})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Expansions) != 1 || r.Expansions[0].Resources[0] != resource {
			t.Fatal("large resource did not roundtrip")
		}
		if cached != "" && cached != r.Expansions[0].ID {
			t.Fatal("cache did not persist")
		}
		cached = r.Expansions[0].ID
	}
	if _, err := c.CloseRender(t.Context(), &CloseRequest{Session: a.Session}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Expand(t.Context(), &ExpandRequest{Session: a.Session}); status.Code(err) != codes.NotFound {
		t.Fatalf("closed session: %v", err)
	}
	if _, err := c.Expand(t.Context(), &ExpandRequest{Session: b.Session}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory survived: %v", err)
	}
}

func TestStartupFailures(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{{"name", "handshake mismatch"}, {"version", "handshake mismatch"}, {"fail", "startup exploded"}, {"notready", "DeadlineExceeded"}} {
		t.Run(tc.mode, func(t *testing.T) {
			c, err := StartWithOptions(t.Context(), helperCommand(t, tc.mode), Options{StartupTimeout: 400 * time.Millisecond, ShutdownTimeout: 100 * time.Millisecond})
			if c != nil {
				_ = c.Close()
				t.Fatal("unexpected client")
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
	_, err := Start(t.Context(), Command{Name: "missing", Command: "/no/such/plugin"})
	if err == nil {
		t.Fatal("missing executable succeeded")
	}
}

func TestDeadlineInvalidatesProcess(t *testing.T) {
	for _, mode := range []string{"slow", "stubborn"} {
		t.Run(mode, func(t *testing.T) {
			c := helperClient(t, mode, Options{RPCTimeout: 100 * time.Millisecond, ShutdownTimeout: 100 * time.Millisecond})
			s, err := c.OpenRender(t.Context(), &OpenRequest{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, err = c.Expand(ctx, &ExpandRequest{Session: s.Session})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline: %v", err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("shutdown exceeded bound")
			}
			select {
			case <-c.done:
			default:
				t.Fatal("process not reaped")
			}
			if _, err := c.OpenRender(t.Context(), &OpenRequest{}); err == nil {
				t.Fatal("uncertain client was reused")
			}
			if err := syscall.Kill(c.process.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("process survived: %v", err)
			}
		})
	}
}

func TestCrashIncludesBoundedStderr(t *testing.T) {
	c := helperClient(t, "crash", Options{ShutdownTimeout: 100 * time.Millisecond})
	s, err := c.OpenRender(t.Context(), &OpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Expand(t.Context(), &ExpandRequest{Session: s.Session})
	if err == nil || !strings.Contains(err.Error(), "CRASH-END") || !strings.Contains(err.Error(), "exit status 25") || len(err.Error()) > stderrLimit+1000 {
		t.Fatalf("crash error missing context or unbounded: %v", err)
	}
}

func TestPayloadLimits(t *testing.T) {
	c := helperClient(t, "normal", Options{})
	s, err := c.OpenRender(t.Context(), &OpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Expand(t.Context(), &ExpandRequest{Session: s.Session, Resources: []Resource{{YAML: strings.Repeat("x", MaxMessageBytes)}}})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized request: %v", err)
	}
	if _, err := c.Expand(t.Context(), &ExpandRequest{Session: s.Session}); err != nil {
		t.Fatalf("undispatched request poisoned session: %v", err)
	}
	large := helperClient(t, "huge-response", Options{})
	s, err = large.OpenRender(t.Context(), &OpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = large.Expand(t.Context(), &ExpandRequest{Session: s.Session})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized response: %v", err)
	}
	if _, err := large.OpenRender(t.Context(), &OpenRequest{}); err == nil {
		t.Fatal("lost response did not invalidate client")
	}
}

func TestConfiguredDeadlineAndParentCancellation(t *testing.T) {
	t.Run("configured RPC deadline", func(t *testing.T) {
		c := helperClient(t, "slow", Options{RPCTimeout: 50 * time.Millisecond, ShutdownTimeout: 100 * time.Millisecond})
		s, err := c.OpenRender(t.Context(), &OpenRequest{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Expand(t.Context(), &ExpandRequest{Session: s.Session})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("configured timeout: %v", err)
		}
		select {
		case <-c.done:
		default:
			t.Fatal("process survived configured timeout")
		}
	})
	t.Run("parent cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		c, err := StartWithOptions(ctx, helperCommand(t, "normal"), Options{ShutdownTimeout: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		cancel()
		select {
		case <-c.closed:
		case <-time.After(time.Second):
			t.Fatal("parent cancellation left client alive")
		}
	})
}

type cancellationEngine struct {
	*testEngine
	entered  chan struct{}
	released chan struct{}
}

func (s *cancellationEngine) Expand(ctx context.Context, r *ExpandRequest) (*ExpandResponse, error) {
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *cancellationEngine) Close() error { close(s.released); return nil }

func TestServeCancelsCallsAndClosesEngine(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "rpc.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &cancellationEngine{testEngine: &testEngine{}, entered: make(chan struct{}), released: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, socket, s) }()
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rpcCtx, cancelRPC := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelRPC()
	request, err := encode(&ExpandRequest{Session: "test"})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- conn.Invoke(rpcCtx, "/"+serviceName+"/Expand", request, new(wrapperspb.BytesValue), grpc.WaitForReady(true))
	}()
	select {
	case <-s.entered:
	case <-rpcCtx.Done():
		t.Fatal("RPC never entered engine")
	}
	cancel()
	select {
	case err := <-result:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("server cancellation: %v", err)
		}
	case <-rpcCtx.Done():
		t.Fatal("server did not cancel engine")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-rpcCtx.Done():
		t.Fatal("server did not stop")
	}
	select {
	case <-s.released:
	default:
		t.Fatal("engine resources not released")
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket survived: %v", err)
	}
}

func TestServerRejectsOversizedWireRequest(t *testing.T) {
	c := helperClient(t, "normal", Options{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// Bypass the typed client's local check to exercise the server's receive
	// bound as well, even against a misbehaving executable/host peer.
	message := wrapperspb.Bytes([]byte(strings.Repeat("x", MaxMessageBytes)))
	err := c.conn.Invoke(ctx, "/"+serviceName+"/Expand", message, new(wrapperspb.BytesValue), grpc.MaxCallSendMsgSize(MaxMessageBytes+100))
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("server receive limit: %v", err)
	}
}
