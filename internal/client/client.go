package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"bht1/internal/hexdump"
	"bht1/internal/wire"
)

const (
	UserAgent   = "bcurl-go/1.0"
	DefaultPort = 9000

	MaxRequests = (wire.MaxStreamID + 1) / 2
)

type Target struct {
	Host string
	Port int
	Path string
}

func (t Target) Authority() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

func ParseURL(raw string) (Target, error) {
	rest := strings.TrimPrefix(raw, "bht://")
	if strings.Contains(rest, "://") {
		return Target{}, fmt.Errorf("unsupported scheme in %q", raw)
	}
	authority, path, hasPath := strings.Cut(rest, "/")
	path = "/" + path
	if !hasPath {
		path = "/"
	}
	host, portText := authority, ""
	switch {
	case strings.HasPrefix(authority, "["):
		end := strings.Index(authority, "]")
		if end < 0 {
			return Target{}, fmt.Errorf("unterminated IPv6 address in %q", raw)
		}
		host = authority[1:end]
		if tail := authority[end+1:]; tail != "" {
			if !strings.HasPrefix(tail, ":") {
				return Target{}, fmt.Errorf("bad authority in %q", raw)
			}
			portText = tail[1:]
		}
	case strings.Count(authority, ":") == 1:
		host, portText, _ = strings.Cut(authority, ":")
	case strings.Count(authority, ":") > 1:
		return Target{}, fmt.Errorf("IPv6 address must be bracketed in %q", raw)
	}
	if host == "" {
		return Target{}, fmt.Errorf("no host in %q", raw)
	}
	port := DefaultPort
	if portText != "" {
		p, err := strconv.Atoi(portText)
		if err != nil || p < 1 || p > 65535 || strings.Trim(portText, "0123456789") != "" {
			return Target{}, fmt.Errorf("bad port in %q", raw)
		}
		port = p
	}
	if err := wire.ValidatePath(path); err != nil {
		return Target{}, fmt.Errorf("%q: %v", raw, err)
	}
	return Target{Host: host, Port: port, Path: path}, nil
}

type Options struct {
	Timeout time.Duration
	MaxData int
	Headers wire.Headers
	Trace   io.Writer
}

type Exchange struct {
	StreamID uint16
	Target   Target
	Method   string
	Response *wire.Response
	Err      error
}

type Result struct {
	Exchanges      []*Exchange
	Errors         []*wire.ErrorMsg
	FramesSent     int
	FramesReceived int
	Connections    int
}

type ConnectError struct{ Err error }

func (e *ConnectError) Error() string { return "connect: " + e.Err.Error() }
func (e *ConnectError) Unwrap() error { return e.Err }

type ProtocolError struct{ Reason string }

func (e *ProtocolError) Error() string { return "protocol error: " + e.Reason }

func Fetch(ctx context.Context, targets []Target, method string, opts Options) (*Result, error) {
	if len(targets) == 0 {
		return nil, errors.New("no targets")
	}
	if len(targets) > MaxRequests {
		return nil, fmt.Errorf("%d requests do not fit in one connection's stream IDs (max %d)", len(targets), MaxRequests)
	}
	authority := targets[0].Authority()
	for _, t := range targets[1:] {
		if t.Authority() != authority {
			return nil, fmt.Errorf("all URLs must share one host:port, got %s and %s", authority, t.Authority())
		}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}

	res := &Result{}
	out := append([]byte{}, wire.Preface...)
	asm := wire.NewAssembler(wire.RoleClient)
	byStream := make(map[uint16]*Exchange, len(targets))
	for i, t := range targets {
		id := uint16(1 + 2*i)
		headers := append(wire.Headers{
			{Name: "host", Value: authority},
			{Name: "user-agent", Value: UserAgent},
		}, opts.Headers...)
		frames, err := wire.EncodeRequest(&wire.Request{Method: method, Path: t.Path, Headers: headers}, id, opts.MaxData)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Path, err)
		}
		for _, f := range frames {
			out = f.AppendTo(out)
		}
		res.FramesSent += len(frames)
		asm.OpenStream(id, method != "HEAD")
		ex := &Exchange{StreamID: id, Target: t, Method: method}
		res.Exchanges = append(res.Exchanges, ex)
		byStream[id] = ex
	}

	tr := newTracer(opts.Trace)
	tr.note("* connecting to %s", authority)
	d := net.Dialer{Timeout: opts.Timeout}
	nc, err := d.DialContext(ctx, "tcp", authority)
	if err != nil {
		return nil, &ConnectError{Err: err}
	}
	res.Connections = 1
	tr.note("* connected to %s", nc.RemoteAddr())
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	defer func() {
		stop()
		nc.Close()
	}()

	tr.out(out)

	writeErr := make(chan error, 1)
	go func() {
		nc.SetWriteDeadline(time.Now().Add(opts.Timeout))
		_, err := nc.Write(out)
		writeErr <- err
	}()

	err = readResponses(nc, asm, byStream, res, opts.Timeout, tr)
	if werr := <-writeErr; err == nil && werr != nil {
		err = &ProtocolError{Reason: "sending requests: " + werr.Error()}
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	tr.note("* %d TCP connection, %d frames out, %d frames in", res.Connections, res.FramesSent, res.FramesReceived)
	return res, err
}

func readResponses(nc net.Conn, asm *wire.Assembler, byStream map[uint16]*Exchange, res *Result, timeout time.Duration, tr *tracer) error {
	r := wire.NewReader(false)
	buf := make([]byte, 64<<10)
	pending := len(byStream)
	for pending > 0 {
		f, ok, err := r.Next()
		if err != nil {
			return &ProtocolError{Reason: err.Error()}
		}
		if !ok {
			nc.SetReadDeadline(time.Now().Add(timeout))
			n, err := nc.Read(buf)
			r.Feed(buf[:n])
			if err != nil && n == 0 {
				if errors.Is(err, io.EOF) {
					return &ProtocolError{Reason: fmt.Sprintf("connection closed with %d response(s) outstanding", pending)}
				}
				return &ProtocolError{Reason: err.Error()}
			}
			continue
		}
		res.FramesReceived++
		tr.in(f)
		m, err := asm.Accept(f)
		var se *wire.StreamError
		switch {
		case errors.As(err, &se):
			if ex := byStream[se.StreamID]; ex != nil && ex.Response == nil && ex.Err == nil {
				ex.Err = se
				pending--
			}
			continue
		case err != nil:
			return &ProtocolError{Reason: err.Error()}
		}
		switch m := m.(type) {
		case *wire.Response:
			byStream[m.StreamID].Response = m
			pending--
		case *wire.ErrorMsg:
			if m.StreamID == 0 {
				res.Errors = append(res.Errors, m)
				return nil
			}
			byStream[m.StreamID].Err = fmt.Errorf("server sent ERROR %d: %s", m.Status, m.Reason)
			pending--
		}
	}
	return nil
}

const (
	ExitOK          = 0
	ExitUsage       = 2
	ExitClientError = 4
	ExitServerError = 5
	ExitConnect     = 7
	ExitProtocol    = 8
)

func ExitCode(res *Result, err error) int {
	var ce *ConnectError
	switch {
	case errors.As(err, &ce):
		return ExitConnect
	case err != nil, res == nil, len(res.Errors) > 0:
		return ExitProtocol
	}
	worst := ExitOK
	for _, ex := range res.Exchanges {
		switch {
		case ex.Err != nil || ex.Response == nil:
			return ExitProtocol
		case ex.Response.Status >= 500:
			worst = max(worst, ExitServerError)
		case ex.Response.Status >= 400:
			worst = max(worst, ExitClientError)
		}
	}
	return worst
}

type tracer struct {
	w       io.Writer
	sent    int
	recv    int
	scratch []byte
}

func newTracer(w io.Writer) *tracer { return &tracer{w: w} }

func (t *tracer) note(format string, args ...any) {
	if t.w != nil {
		fmt.Fprintf(t.w, format+"\n", args...)
	}
}

func (t *tracer) out(raw []byte) {
	if t.w == nil {
		return
	}
	t.dump("> ", raw, t.sent)
	t.sent += len(raw)
}

func (t *tracer) in(f wire.Frame) {
	if t.w == nil {
		return
	}
	t.scratch = f.AppendAsReceived(t.scratch[:0])
	t.dump("< ", t.scratch, t.recv)
	t.recv += len(t.scratch)
}

func (t *tracer) dump(prefix string, raw []byte, base int) {
	text := hexdump.Render(hexdump.Annotate(raw, base), 16)
	for line := range strings.SplitSeq(text, "\n") {
		fmt.Fprintln(t.w, prefix+line)
	}
}
