package crossplanerender

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// functionTrace records actual function responses, not an interpretation of
// engine-generated names. The final desired resource map is cumulative across
// pipeline steps, so the latest response is authoritative for declared names.
type functionTrace struct {
	mu                sync.Mutex
	declaredNames     map[string]string
	evidence          []json.RawMessage
	resourceSelectors []*fnv1.ResourceSelector
	schemaSelectors   []*fnv1.SchemaSelector
}

func (t *functionTrace) record(name string, rsp *fnv1.RunFunctionResponse) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.declaredNames = map[string]string{}
	legacy := rsp.GetRequirements().GetExtraResources() //nolint:staticcheck // Legacy Functions still populate extra_resources; retain their dependency witnesses.
	for _, selectors := range []map[string]*fnv1.ResourceSelector{
		rsp.GetRequirements().GetResources(), legacy,
	} {
		for _, selector := range selectors {
			if selector != nil {
				copy := &fnv1.ResourceSelector{}
				proto.Merge(copy, selector)
				t.resourceSelectors = append(t.resourceSelectors, copy)
			}
		}
	}
	for _, selector := range rsp.GetRequirements().GetSchemas() {
		if selector != nil {
			copy := &fnv1.SchemaSelector{}
			proto.Merge(copy, selector)
			t.schemaSelectors = append(t.schemaSelectors, copy)
		}
	}
	for key, desired := range rsp.GetDesired().GetResources() {
		o := desired.GetResource().AsMap()
		metadata, _ := o["metadata"].(map[string]any)
		n, _ := metadata["name"].(string)
		t.declaredNames[key] = n
	}
	// Only the latest function evidence is needed for the current reconcile.
	// Credentials and observed/required resource contents are not copied here.
	raw := map[string]any{"function": name}
	for key, value := range map[string]proto.Message{
		"context": rsp.GetContext(), "requirements": rsp.GetRequirements(),
	} {
		data, err := protojson.Marshal(value)
		if err == nil && string(data) != "null" {
			raw[key] = json.RawMessage(data)
		}
	}
	results := []json.RawMessage{}
	for _, result := range rsp.GetResults() {
		if data, err := protojson.Marshal(result); err == nil {
			results = append(results, data)
		}
	}
	raw["results"] = results
	if data, err := json.Marshal(raw); err == nil {
		t.evidence = append(t.evidence, data)
	}
}

type functionProxy struct {
	fnv1.UnimplementedFunctionRunnerServiceServer
	name   string
	conn   *grpc.ClientConn
	trace  *functionTrace
	server *grpc.Server
	done   chan struct{}
}

func startProxy(name, target string, trace *functionTrace) (*functionProxy, string, error) {
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true), grpc.MaxCallRecvMsgSize(16<<20)),
	)
	if err != nil {
		return nil, "", fmt.Errorf("connect Function %q: %w", name, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = conn.Close()
		return nil, "", fmt.Errorf("listen for Function %q: %w", name, err)
	}
	p := &functionProxy{
		name: name, conn: conn, trace: trace, done: make(chan struct{}),
		server: grpc.NewServer(grpc.MaxRecvMsgSize(16<<20), grpc.MaxSendMsgSize(16<<20)),
	}
	fnv1.RegisterFunctionRunnerServiceServer(p.server, p)
	go func() {
		defer close(p.done)
		_ = p.server.Serve(listener)
	}()
	return p, listener.Addr().String(), nil
}

func (p *functionProxy) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	rsp := &fnv1.RunFunctionResponse{}
	err := p.conn.Invoke(ctx, fnv1.FunctionRunnerService_RunFunction_FullMethodName, req, rsp)
	if status.Code(err) == codes.Unimplemented {
		// v1 and v1beta1 share identical wire messages. Keep official legacy
		// fallback semantics without importing the controller implementation.
		err = p.conn.Invoke(ctx, "/apiextensions.fn.proto.v1beta1.FunctionRunnerService/RunFunction", req, rsp)
	}
	if err != nil {
		return nil, err
	}
	p.trace.record(p.name, rsp)
	return rsp, nil
}

func (p *functionProxy) close() {
	p.server.Stop()
	_ = p.conn.Close()
	<-p.done
}
