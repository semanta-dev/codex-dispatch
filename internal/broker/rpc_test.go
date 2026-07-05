package broker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParseRequestHappyPath(t *testing.T) {
	line := `{"jsonrpc":"2.0","method":"broker.ping","params":{},"id":1}`
	req, err := ParseRequest([]byte(line))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if req.Method != "broker.ping" {
		t.Fatalf("Method = %q, want broker.ping", req.Method)
	}
	if req.ID == nil {
		t.Fatalf("ID should be non-nil for a request")
	}
}

func TestParseRequestRejectsMissingVersion(t *testing.T) {
	_, err := ParseRequest([]byte(`{"method":"x","id":1}`))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestParseRequestRejectsWrongVersion(t *testing.T) {
	_, err := ParseRequest([]byte(`{"jsonrpc":"1.0","method":"x","id":1}`))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestParseRequestRejectsMissingMethod(t *testing.T) {
	_, err := ParseRequest([]byte(`{"jsonrpc":"2.0","id":1}`))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestParseRequestRejectsMalformedJSON(t *testing.T) {
	_, err := ParseRequest([]byte(`{not json`))
	if !errors.Is(err, ErrParseError) {
		t.Fatalf("err = %v, want ErrParseError", err)
	}
}

func TestMarshalSuccessIncludesProtocolVersion(t *testing.T) {
	id := json.RawMessage(`1`)
	out, err := MarshalSuccess(&id, map[string]any{"hello": "world"})
	if err != nil {
		t.Fatalf("MarshalSuccess: %v", err)
	}
	if !bytes.Contains(out, []byte(`"_protocol_version":"1"`)) {
		t.Fatalf("missing _protocol_version: %s", out)
	}
	if !bytes.HasSuffix(out, []byte("\n")) {
		t.Fatalf("response must end with newline: %q", out)
	}
}

func TestMarshalErrorWiresStandardEnvelope(t *testing.T) {
	id := json.RawMessage(`1`)
	out, err := MarshalError(&id, -32601, "method not found", nil)
	if err != nil {
		t.Fatalf("MarshalError: %v", err)
	}
	if !bytes.Contains(out, []byte(`"code":-32601`)) {
		t.Fatalf("missing code: %s", out)
	}
	if !bytes.Contains(out, []byte(`"message":"method not found"`)) {
		t.Fatalf("missing message: %s", out)
	}
}

func TestReadLineRespectsMaxSize(t *testing.T) {
	huge := strings.Repeat("a", 5*1024*1024) // 5 MiB > 4 MiB cap
	r := bufio.NewReader(bytes.NewReader([]byte(huge + "\n")))
	_, err := ReadLine(r)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
}

func TestReadLineAcceptsAtCap(t *testing.T) {
	// 4 MiB minus a newline byte fits exactly.
	atCap := strings.Repeat("a", 4*1024*1024-1)
	r := bufio.NewReader(bytes.NewReader([]byte(atCap + "\n")))
	line, err := ReadLine(r)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if len(line) != 4*1024*1024-1 {
		t.Fatalf("len = %d, want %d", len(line), 4*1024*1024-1)
	}
}

func TestReadLineStripsCRLF(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte("foo\r\n")))
	line, err := ReadLine(r)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if string(line) != "foo" {
		t.Fatalf("line = %q, want %q", line, "foo")
	}
}

func TestReadLineReturnsPartialLineAtEOF(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte("hello")))
	line, err := ReadLine(r)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if string(line) != "hello" {
		t.Fatalf("line = %q, want %q", line, "hello")
	}
	_, err = ReadLine(r)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestReadLineReturnsTerminatedThenPartialLine(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte("foo\nbar")))
	line, err := ReadLine(r)
	if err != nil {
		t.Fatalf("first ReadLine: %v", err)
	}
	if string(line) != "foo" {
		t.Fatalf("first line = %q, want %q", line, "foo")
	}
	line, err = ReadLine(r)
	if err != nil {
		t.Fatalf("second ReadLine: %v", err)
	}
	if string(line) != "bar" {
		t.Fatalf("second line = %q, want %q", line, "bar")
	}
	_, err = ReadLine(r)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}
