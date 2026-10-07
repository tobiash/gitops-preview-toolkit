package crossplanerender

import (
	"context"
	"net"
	"testing"
	"time"

	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

type fixtureFunction struct {
	fnv1.UnimplementedFunctionRunnerServiceServer
}

func (fixtureFunction) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	// Echo the genuine request pipeline state, adding one explicitly named
	// and one unnamed output. This tests forwarding, not render semantics.
	named, err := structpb.NewStruct(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "explicit"},
	})
	if err != nil {
		return nil, err
	}
	unnamed, err := structpb.NewStruct(map[string]any{"apiVersion": "v1", "kind": "ConfigMap"})
	if err != nil {
		return nil, err
	}
	return &fnv1.RunFunctionResponse{Desired: &fnv1.State{Resources: map[string]*fnv1.Resource{
		"named": {Resource: named}, "unnamed": {Resource: unnamed},
	}}, Context: req.GetContext()}, nil
}

func TestProxyForwardsActualGRPCAndRecordsNaming(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	fnv1.RegisterFunctionRunnerServiceServer(server, fixtureFunction{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() { server.Stop(); <-done })
	trace := &functionTrace{}
	proxy, target, err := startProxy("fixture", listener.Addr().String(), trace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.close)
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	contextData, err := structpb.NewStruct(map[string]any{"check": "forwarded"})
	if err != nil {
		t.Fatal(err)
	}
	rsp, err := fnv1.NewFunctionRunnerServiceClient(conn).RunFunction(ctx,
		&fnv1.RunFunctionRequest{Context: contextData}, grpc.WaitForReady(true))
	if err != nil {
		t.Fatal(err)
	}
	if rsp.GetContext().AsMap()["check"] != "forwarded" {
		t.Fatal("proxy mutated function context")
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.declaredNames["named"] != "explicit" {
		t.Fatal("explicit name not recorded")
	}
	if name, present := trace.declaredNames["unnamed"]; !present || name != "" {
		t.Fatal("unnamed output not recorded")
	}
	if len(trace.evidence) != 1 {
		t.Fatal("function context evidence missing")
	}
}
