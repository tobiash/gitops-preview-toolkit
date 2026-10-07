package agentmcp

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestFrameReader(t *testing.T) {
	exact := `{"x":"` + strings.Repeat("x", maxFrameBytes-len(`{"x":""}`)-1) + "\"}\n"
	for _, tt := range []struct {
		name, input string
		valid       bool
	}{
		{"lines", "{\"x\":1}\n{\"x\":2}\r\n", true},
		{"eof", `{"x":1}`, true},
		{"empty", "", true},
		{"boundary", exact, true},
		{"oversize", strings.TrimSuffix(exact, "\n") + " \n", false},
		{"unterminated", `{"x":"` + strings.Repeat("CANARY", 1<<18), false},
		{"multiline", "{\n\"x\":1}\n", false},
		{"syntax", "{CANARY}\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := newFrameReader(io.NopCloser(strings.NewReader(tt.input)))
			var output bytes.Buffer
			_, err := io.CopyBuffer(&output, reader, make([]byte, 7))
			if (err == nil) != tt.valid {
				t.Fatalf("Read error=%v want valid=%v", err, tt.valid)
			}
			if tt.valid && output.String() != tt.input {
				t.Fatal("Read altered valid framing")
			}
			if !tt.valid && (output.Len() != 0 || strings.Contains(err.Error(), "CANARY") || len(err.Error()) > 128) {
				t.Fatal("Read exposed invalid frame")
			}
		})
	}
}

type endlessFrame struct{ read int }

func (r *endlessFrame) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.read += len(p)
	return len(p), nil
}
func (*endlessFrame) Close() error { return nil }

func TestFrameReaderStopsBeforeUnboundedDecode(t *testing.T) {
	input := &endlessFrame{}
	r := newFrameReader(input)
	if n, err := r.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("Read=%d,%v want immediate frame failure", n, err)
	}
	if input.read > maxFrameBytes+1 {
		t.Fatalf("consumed %d bytes before rejecting frame", input.read)
	}
}
