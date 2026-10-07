package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// MaxMessageBytes bounds each serialized protobuf request and response (64 MiB).
const MaxMessageBytes = 64 << 20

const serviceName = "flux.preview.plugin.v1.RenderPlugin"

// The envelope versions every message, not just the initial handshake. Payload
// types are the corresponding request/response types in types.go.
type envelope struct {
	ProtocolVersion int             `json:"protocolVersion"`
	Payload         json.RawMessage `json:"payload"`
}

func encode(value any) (*wrapperspb.BytesValue, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode plugin payload: %w", err)
	}
	data, err := json.Marshal(envelope{ProtocolVersion: ProtocolVersion, Payload: payload})
	if err != nil {
		return nil, fmt.Errorf("encode plugin envelope: %w", err)
	}
	message := wrapperspb.Bytes(data)
	if proto.Size(message) > MaxMessageBytes {
		return nil, status.Error(codes.ResourceExhausted, "plugin message exceeds 64 MiB")
	}
	return message, nil
}

func decode(message *wrapperspb.BytesValue, value any) error {
	if message == nil || proto.Size(message) > MaxMessageBytes {
		return status.Error(codes.ResourceExhausted, "plugin message exceeds 64 MiB or is absent")
	}
	var e envelope
	if err := json.Unmarshal(message.Value, &e); err != nil {
		return status.Error(codes.InvalidArgument, "invalid plugin JSON envelope")
	}
	if e.ProtocolVersion != ProtocolVersion {
		return status.Errorf(codes.FailedPrecondition, "plugin protocol version %d, expected %d", e.ProtocolVersion, ProtocolVersion)
	}
	if len(e.Payload) == 0 || string(e.Payload) == "null" {
		return status.Error(codes.InvalidArgument, "missing plugin payload")
	}
	if err := json.Unmarshal(e.Payload, value); err != nil {
		return status.Error(codes.InvalidArgument, "invalid plugin payload")
	}
	return nil
}

func rpcMethod[Req, Resp any](name string, call func(Service, context.Context, *Req) (*Resp, error)) grpc.MethodDesc {
	return grpc.MethodDesc{MethodName: name, Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		message := new(wrapperspb.BytesValue)
		if err := dec(message); err != nil {
			return nil, err
		}
		handler := func(ctx context.Context, input any) (any, error) {
			var request Req
			if err := decode(input.(*wrapperspb.BytesValue), &request); err != nil {
				return nil, err
			}
			response, err := call(srv.(Service), ctx, &request)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil, status.FromContextError(err).Err()
				}
				return nil, err
			}
			if response == nil {
				return nil, status.Error(codes.Internal, "plugin returned a nil response")
			}
			return encode(response)
		}
		if interceptor == nil {
			return handler(ctx, message)
		}
		return interceptor(ctx, message, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + serviceName + "/" + name}, handler)
	}}
}

var renderServiceDesc = grpc.ServiceDesc{
	ServiceName: serviceName,
	HandlerType: (*Service)(nil),
	Methods: []grpc.MethodDesc{
		rpcMethod("Describe", func(s Service, ctx context.Context, r *DescribeRequest) (*DescribeResponse, error) {
			if r.ProtocolVersion != ProtocolVersion {
				return nil, status.Errorf(codes.FailedPrecondition, "unsupported requested protocol %d", r.ProtocolVersion)
			}
			return s.Describe(ctx, r)
		}),
		rpcMethod("OpenRender", Service.OpenRender),
		rpcMethod("Expand", Service.Expand),
		rpcMethod("CloseRender", Service.CloseRender),
	},
	Metadata: "api/plugin/render.proto",
}
