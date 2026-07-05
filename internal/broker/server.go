package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Handler is a function that handles one JSON-RPC method call.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Server is the broker's localhost HTTP JSON-RPC server.
type Server struct {
	listenAddr string
	addrFile   string

	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewServer constructs a Server bound to listenAddr. Use "127.0.0.1:0" to
// let the OS choose a port. Tests that drive serveConn directly may pass "".
func NewServer(listenAddr string) *Server {
	return &Server{listenAddr: listenAddr, handlers: map[string]Handler{}}
}

// SetAddrFile makes Serve write its actual TCP address to path once listening.
// The file is removed when Serve returns.
func (s *Server) SetAddrFile(path string) {
	s.addrFile = path
}

// HandleFunc registers a handler for the given method name. Safe to call
// before Serve; concurrent calls during Serve are also safe (RWMutex).
func (s *Server) HandleFunc(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// Serve listens on localhost TCP and serves newline-delimited JSON-RPC over
// HTTP POST /rpc until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if s.addrFile != "" {
		addr := listener.Addr().String()
		if err := writeAddrFileAtomic(s.addrFile, addr); err != nil {
			listener.Close()
			return fmt.Errorf("write broker addr: %w", err)
		}
		defer removeAddrFileIfCurrent(s.addrFile, addr)
	}
	defer listener.Close()

	httpSrv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	err = httpSrv.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	return err
}

func writeAddrFileAtomic(path, addr string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write([]byte(addr + "\n")); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func removeAddrFileIfCurrent(path, addr string) {
	b, err := os.ReadFile(path)
	if err == nil && string(b) == addr+"\n" {
		_ = os.Remove(path)
	}
}

// ServeHTTP handles one JSON-RPC request per HTTP request. The response body is
// newline-delimited JSON: zero or more notifications followed by one response.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/rpc" {
		http.NotFound(w, r)
		return
	}
	defer r.Body.Close()
	line, err := io.ReadAll(io.LimitReader(r.Body, MaxMessageSize+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(line) > MaxMessageSize {
		http.Error(w, ErrMessageTooLarge.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	n := &httpNotifier{w: w}
	s.dispatch(r.Context(), n, n, line)
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)
	// Per-connection write mutex. Streaming handlers push task.event
	// notifications through this same mutex so concurrent
	// notification + response writes never interleave.
	var writeMu sync.Mutex
	reader := bufio.NewReaderSize(conn, 64*1024)
	for {
		if ctx.Err() != nil {
			return
		}
		line, err := ReadLine(reader)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, io.EOF) {
				return
			}
			if errors.Is(err, ErrMessageTooLarge) {
				resp, _ := MarshalError(nil, -32700, err.Error(), nil)
				writeMu.Lock()
				_, _ = conn.Write(resp)
				writeMu.Unlock()
				return
			}
			resp, _ := MarshalError(nil, -32700, err.Error(), nil)
			writeMu.Lock()
			_, _ = conn.Write(resp)
			writeMu.Unlock()
			return
		}
		n := &connNotifier{conn: conn, mu: &writeMu}
		s.dispatch(ctx, n, n, line)
	}
}

type responseWriter interface {
	Write([]byte) (int, error)
}

func (s *Server) dispatch(ctx context.Context, out responseWriter, notifier Notifier, line []byte) {
	req, err := ParseRequest(line)
	if err != nil {
		// Parse / invalid-request errors ALWAYS get an error response, even
		// though they carry no usable id (the client could not have framed a
		// notification it never sent). Per JSON-RPC 2.0 the id is null. The
		// client surfaces these (see Client read loops) instead of hanging on a
		// silent malformed line.
		code := -32700
		if errors.Is(err, ErrInvalidRequest) {
			code = -32600
		}
		resp, _ := MarshalError(nil, code, err.Error(), nil)
		_, _ = out.Write(resp)
		return
	}

	// A JSON-RPC NOTIFICATION is a well-formed request with no id. The spec
	// forbids any response to a notification — success, method-not-found, and
	// handler errors are all suppressed. We still invoke the handler for its
	// side effects (when one is registered) but write nothing back.
	isNotification := req.ID == nil

	s.mu.RLock()
	h, ok := s.handlers[req.Method]
	s.mu.RUnlock()
	if !ok {
		if isNotification {
			return // no response to a notification, even for an unknown method
		}
		resp, _ := MarshalError(req.ID, -32601, "method not found: "+req.Method, nil)
		_, _ = out.Write(resp)
		return
	}

	streamCtx := WithNotifier(ctx, notifier)
	result, err := h(streamCtx, req.Params)
	if isNotification {
		return // handler ran for side effects; a notification gets no reply
	}
	if err != nil {
		code := -32603
		msg := err.Error()
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			code = rpcErr.Code
			msg = rpcErr.Message
		}
		resp, _ := MarshalError(req.ID, code, msg, nil)
		_, _ = out.Write(resp)
		return
	}
	resp, err := MarshalSuccess(req.ID, result)
	if err != nil {
		resp, _ := MarshalError(req.ID, -32603, err.Error(), nil)
		_, _ = out.Write(resp)
		return
	}
	_, _ = out.Write(resp)
}

// Notifier is the interface streaming handlers use to push JSON-RPC
// notifications back to the client during a long-running call.
type Notifier interface {
	Notify(method string, params any) error
}

type notifierKey struct{}

// WithNotifier wraps a Notifier into the context. The server passes this
// context to every handler; streaming handlers retrieve the notifier via
// NotifierFrom to push events back to the client.
func WithNotifier(ctx context.Context, n Notifier) context.Context {
	return context.WithValue(ctx, notifierKey{}, n)
}

// NotifierFrom retrieves a Notifier from the context (or nil).
func NotifierFrom(ctx context.Context) Notifier {
	n, _ := ctx.Value(notifierKey{}).(Notifier)
	return n
}

// connNotifier is the per-connection Notifier wired into the request
// context. Writes go through the same per-connection mutex used for
// JSON-RPC responses to keep the wire stream serialised.
type connNotifier struct {
	conn net.Conn
	mu   *sync.Mutex
}

func (n *connNotifier) Notify(method string, params any) error {
	frames, err := MarshalNotificationFrames(method, params)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, b := range frames {
		if _, err := n.conn.Write(b); err != nil {
			return err
		}
	}
	return nil
}

func (n *connNotifier) Write(b []byte) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.conn.Write(b)
}

type httpNotifier struct {
	w http.ResponseWriter
}

func (n *httpNotifier) Notify(method string, params any) error {
	frames, err := MarshalNotificationFrames(method, params)
	if err != nil {
		return err
	}
	for _, b := range frames {
		if _, err := n.Write(b); err != nil {
			return err
		}
	}
	return nil
}

func (n *httpNotifier) Write(b []byte) (int, error) {
	written, err := n.w.Write(b)
	if err != nil {
		return written, err
	}
	if f, ok := n.w.(http.Flusher); ok {
		f.Flush()
	}
	return written, nil
}
