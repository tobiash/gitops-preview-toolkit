package agentmcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxFrameBytes = 128 << 10

// StdioTransport bounds each incoming JSON-RPC frame to 128 KiB, including its
// newline, before the SDK decodes it. Invalid/oversized frames end the session
// with a fixed error and are never partially forwarded to the SDK.
func StdioTransport() *mcp.IOTransport {
	return &mcp.IOTransport{Reader: newFrameReader(os.Stdin), Writer: stdoutWriter{os.Stdout}}
}

type stdoutWriter struct{ io.Writer }

func (stdoutWriter) Close() error { return nil }

type frameReader struct {
	io.ReadCloser
	reader  *bufio.Reader
	pending []byte
	err     error
}

func newFrameReader(r io.ReadCloser) *frameReader {
	return &frameReader{ReadCloser: r, reader: bufio.NewReaderSize(r, maxFrameBytes+1)}
}

func (r *frameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		line, err := r.reader.ReadSlice('\n')
		switch {
		case len(line) > maxFrameBytes || errors.Is(err, bufio.ErrBufferFull):
			r.err = errors.New("MCP frame exceeds 128 KiB")
		case err != nil && !errors.Is(err, io.EOF):
			r.err = errors.New("MCP input read failed")
		case len(line) == 0 && errors.Is(err, io.EOF):
			r.err = io.EOF
		case !json.Valid(line):
			// Also prevents multiline JSON from bypassing the per-line budget.
			r.err = errors.New("invalid MCP JSON frame")
		default:
			r.pending = line
			r.err = err
		}
		if len(r.pending) == 0 {
			return 0, r.err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
