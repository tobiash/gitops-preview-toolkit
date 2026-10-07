package plugin

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestEnvelopeValidation(t *testing.T) {
	for _, tc := range []struct {
		name, json string
		code       codes.Code
	}{
		{"malformed", "{", codes.InvalidArgument},
		{"version", `{"protocolVersion":2,"payload":{}}`, codes.FailedPrecondition},
		{"absent", `{"protocolVersion":1}`, codes.InvalidArgument},
		{"null", `{"protocolVersion":1,"payload":null}`, codes.InvalidArgument},
		{"type", `{"protocolVersion":1,"payload":{"session":123}}`, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out OpenResponse
			if err := decode(wrapperspb.Bytes([]byte(tc.json)), &out); status.Code(err) != tc.code {
				t.Fatalf("got %v, want %v", err, tc.code)
			}
		})
	}
}

func TestBoundedStderr(t *testing.T) {
	b := new(boundedStderr)
	_, _ = b.Write([]byte(strings.Repeat("a", stderrLimit-1)))
	_, _ = b.Write([]byte("end"))
	if got := b.String(); len(got) != stderrLimit || !strings.HasSuffix(got, "end") {
		t.Fatal("stderr tail not bounded")
	}
	_, _ = b.Write([]byte(strings.Repeat("b", stderrLimit+100)))
	if got := b.String(); got != strings.Repeat("b", stderrLimit) {
		t.Fatal("large write not bounded")
	}
}
