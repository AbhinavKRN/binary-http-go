package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"bht1/internal/wire"
)

func startServer(t *testing.T, root string, tweak func(*Config)) *Server {
	t.Helper()
	cfg := Config{Root: root, Addr: "127.0.0.1:0", IdleTimeout: 5 * time.Second}
	if tweak != nil {
		tweak(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s.Shutdown(ctx)
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return s
}

type peer struct {
	t  *testing.T
	nc net.Conn
	r  *wire.Reader
}

func dial(t *testing.T, s *Server, preface bool) *peer {
	t.Helper()
	nc, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	p := &peer{t: t, nc: nc, r: wire.NewReader(false)}
	if preface {
		p.write(wire.Preface)
	}
	return p
}

func (p *peer) write(b []byte) {
	p.t.Helper()
	if _, err := p.nc.Write(b); err != nil {
		p.t.Fatal(err)
	}
}

func (p *peer) request(method, path string, id uint16) {
	p.t.Helper()
	frames, err := wire.EncodeRequest(&wire.Request{Method: method, Path: path, Headers: wire.Headers{{Name: "host", Value: "test"}}}, id, 0)
	if err != nil {
		p.t.Fatal(err)
	}
	for _, f := range frames {
		p.write(f.Encode())
	}
}

func (p *peer) frame() (wire.Frame, error) {
	buf := make([]byte, 32<<10)
	for {
		f, ok, err := p.r.Next()
		if err != nil {
			return wire.Frame{}, err
		}
		if ok {
			f.Payload = append([]byte(nil), f.Payload...)
			return f, nil
		}
		p.nc.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, err := p.nc.Read(buf)
		p.r.Feed(buf[:n])
		if err != nil && n == 0 {
			return wire.Frame{}, err
		}
	}
}

func (p *peer) response(id uint16, expectBody bool) (*wire.Response, []wire.Frame) {
	p.t.Helper()
	a := wire.NewAssembler(wire.RoleClient)
	a.OpenStream(id, expectBody)
	var frames []wire.Frame
	for {
		f, err := p.frame()
		if err != nil {
			p.t.Fatalf("waiting for stream %d: %v", id, err)
		}
		frames = append(frames, f)
		m, err := a.Accept(f)
		if err != nil {
			p.t.Fatalf("server sent an invalid response: %v", err)
		}
		if resp, ok := m.(*wire.Response); ok {
			return resp, frames
		}
	}
}

func (p *peer) expectClosed() {
	p.t.Helper()
	if f, err := p.frame(); err == nil {
		p.t.Fatalf("expected the connection to close, got a %s frame", f.Type)
	} else if !errors.Is(err, io.EOF) && !isReset(err) {
		p.t.Fatalf("expected EOF, got %v", err)
	}
}

func (p *peer) expectOpen() {
	p.t.Helper()
	p.request("GET", "/hello.txt", 0xfffd)
	if resp, _ := p.response(0xfffd, true); resp.Status != 200 {
		p.t.Fatalf("connection is not usable: status %d", resp.Status)
	}
}

func isReset(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && !op.Timeout()
}

func header(t *testing.T, r *wire.Response, name string) string {
	t.Helper()
	v, _ := r.Headers.Get(name)
	return v
}

func TestServesAFileWithStaticTableHeaders(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	p := dial(t, s, true)
	p.request("GET", "/hello.txt", 1)
	resp, _ := p.response(1, true)
	if resp.Status != 200 || string(resp.Body) != "Hello, world!\n" {
		t.Fatalf("%d %q", resp.Status, resp.Body)
	}
	var names []string
	for _, h := range resp.Headers {
		names = append(names, h.Name)
		if wire.StaticIndex(h.Name) == 0 {
			t.Errorf("header %q costs a literal name on every response", h.Name)
		}
	}
	if got := header(t, resp, "content-type"); got != "text/plain" {
		t.Errorf("content-type %q", got)
	}
	if _, err := time.Parse(httpDate, header(t, resp, "last-modified")); err != nil {
		t.Errorf("last-modified: %v", err)
	}
}

func TestKeepAliveAcrossManySequentialRequests(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	p := dial(t, s, true)
	for i := range 20 {
		id := uint16(1 + 2*i)
		p.request("GET", "/hello.txt", id)
		if resp, _ := p.response(id, true); resp.StreamID != id || resp.Status != 200 {
			t.Fatalf("request %d: %+v", i, resp)
		}
	}
	if got := s.Stats().Accepted; got != 1 {
		t.Fatalf("%d connections accepted, want 1", got)
	}
}

func TestPipelinedRequestsInOneWrite(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	p := dial(t, s, false)
	blob := append([]byte{}, wire.Preface...)
	paths := []string{"/hello.txt", "/docs/nested.txt", "/", "/missing", "/a%20b.txt", "/docs/"}
	for i, path := range paths {
		frames, _ := wire.EncodeRequest(&wire.Request{Method: "GET", Path: path}, uint16(1+2*i), 0)
		for _, f := range frames {
			blob = f.AppendTo(blob)
		}
	}
	p.write(blob)
	want := []int{200, 200, 200, 404, 200, 200}
	for i := range paths {
		resp, _ := p.response(uint16(1+2*i), true)
		if resp.Status != want[i] {
			t.Errorf("%s: %d, want %d", paths[i], resp.Status, want[i])
		}
	}
}

func TestOneByteAtATime(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	p := dial(t, s, false)
	raw := append([]byte{}, wire.Preface...)
	frames, _ := wire.EncodeRequest(&wire.Request{Method: "GET", Path: "/hello.txt"}, 1, 0)
	raw = frames[0].AppendTo(raw)
	for _, b := range raw {
		p.write([]byte{b})
		time.Sleep(time.Millisecond)
	}
	if resp, _ := p.response(1, true); resp.Status != 200 {
		t.Fatal(resp.Status)
	}
}

func TestLargeBodyIsStreamedInSeveralDataFrames(t *testing.T) {
	root := docRoot(t)
	payload := bytes.Repeat([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0xff}, 30_000)
	if err := os.WriteFile(filepath.Join(root, "big.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	s := startServer(t, root, nil)
	p := dial(t, s, true)
	p.request("GET", "/big.bin", 1)
	resp, frames := p.response(1, true)
	if !bytes.Equal(resp.Body, payload) {
		t.Fatalf("body of %d bytes, want %d", len(resp.Body), len(payload))
	}
	if len(frames) < 1+len(payload)/wire.MaxFrameSize {
		t.Fatalf("%d frames for %d bytes", len(frames), len(payload))
	}
	for _, f := range frames[1:] {
		if f.Type != wire.TypeData || len(f.Payload) > wire.MaxFrameSize {
			t.Fatalf("bad body frame %+v", f.Type)
		}
	}
	p.expectOpen()
}

func TestHeadCarriesLengthButNoData(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	p := dial(t, s, true)
	p.request("HEAD", "/hello.txt", 1)
	resp, frames := p.response(1, false)
	if len(frames) != 1 || header(t, resp, "content-length") != "14" {
		t.Fatalf("%d frames, content-length %q", len(frames), header(t, resp, "content-length"))
	}
	p.request("HEAD", "/missing", 3)
	if resp, frames := p.response(3, false); resp.Status != 404 || len(frames) != 1 {
		t.Fatalf("HEAD 404: %d with %d frames", resp.Status, len(frames))
	}
}

func TestEmptyFileIsOneFrame(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	p := dial(t, s, true)
	p.request("GET", "/empty.bin", 1)
	if resp, frames := p.response(1, true); len(frames) != 1 || header(t, resp, "content-length") != "0" {
		t.Fatalf("%d frames", len(frames))
	}
}

func TestStreamLevelAnswersKeepTheConnection(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	cases := []struct {
		method, path string
		status       int
	}{
		{"GET", "/missing", 404},
		{"POST", "/hello.txt", 405},
		{"DELETE", "/hello.txt", 405},
		{"GET", "/con", 400},
		{"GET", "/hello.txt::$DATA", 400},
	}
	p := dial(t, s, true)
	for i, c := range cases {
		id := uint16(1 + 2*i)
		p.request(c.method, c.path, id)
		if resp, _ := p.response(id, true); resp.Status != c.status {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, resp.Status, c.status)
		}
	}
	p.expectOpen()
}

func TestMalformedHeadIs400OnItsStream(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	p := dial(t, s, true)

	p.write([]byte{0, 0, 0x0b, 1, 1, 0, 0, 1, 1, 0, 7, '/', '.', '.', '/', 'e', 't', 'c', 0})
	p.write([]byte{0, 0, 0x08, 1, 1, 0, 0, 3, 1, 0, 4, '/', 'a', '\\', 'b', 0})
	for _, id := range []uint16{1, 3} {
		if resp, _ := p.response(id, true); resp.Status != 400 {
			t.Errorf("stream %d: %d", id, resp.Status)
		}
	}
	p.expectOpen()
}

func TestRejectedBodyIsDrainedNotFatal(t *testing.T) {
	s := startServer(t, docRoot(t), func(c *Config) { c.MaxBody = 16 })
	p := dial(t, s, true)
	frames, _ := wire.EncodeRequest(&wire.Request{Method: "PUT", Path: "/x", Body: bytes.Repeat([]byte("z"), 100)}, 1, 10)
	for _, f := range frames {
		p.write(f.Encode())
	}
	if resp, _ := p.response(1, true); resp.Status != 413 {
		t.Fatalf("status %d, want 413", resp.Status)
	}
	p.expectOpen()
}

func TestUnknownFramesAreSkipped(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	p := dial(t, s, true)
	p.write(wire.Frame{Type: 0x7f, Payload: []byte{0xde, 0xad}}.Encode())
	p.request("GET", "/hello.txt", 1)
	p.write(wire.Frame{Type: 0x05, StreamID: 1, Payload: bytes.Repeat([]byte{1}, 10_000)}.Encode())
	p.write(wire.Frame{Type: 0xff}.Encode())
	p.request("GET", "/hello.txt", 3)
	for _, id := range []uint16{1, 3} {
		if resp, _ := p.response(id, true); resp.Status != 200 {
			t.Fatalf("stream %d: %d", id, resp.Status)
		}
	}
	if got := s.Stats().SkippedUnknown; got != 3 {
		t.Fatalf("skipped %d, want 3", got)
	}
}

func TestDirtyReservedByteAndFlagsAreIgnored(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	p := dial(t, s, true)
	frames, _ := wire.EncodeRequest(&wire.Request{Method: "GET", Path: "/hello.txt"}, 1, 0)
	raw := frames[0].Encode()
	raw[4] |= 0xfe
	raw[5] = 0xff
	p.write(raw)
	if resp, _ := p.response(1, true); resp.Status != 200 {
		t.Fatal(resp.Status)
	}
}

func expectErrorThenClose(t *testing.T, p *peer) {
	t.Helper()
	f, err := p.frame()
	if err != nil {
		t.Fatalf("no ERROR frame before close: %v", err)
	}
	if f.Type != wire.TypeError || f.StreamID != 0 {
		t.Fatalf("got %s on stream %d, want ERROR on stream 0", f.Type, f.StreamID)
	}
	if status := int(f.Payload[0])<<8 | int(f.Payload[1]); status != 400 {
		t.Fatalf("ERROR status %d", status)
	}
	p.expectClosed()
}

func TestConnectionErrors(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	cases := map[string][]byte{
		"bad preface":         []byte("GET / HTTP/1.1\r\n\r\n"),
		"oversized length":    append(append([]byte{}, wire.Preface...), 0xff, 0xff, 0xff, 1, 1, 0, 0, 1),
		"request on stream 0": append(append([]byte{}, wire.Preface...), 0, 0, 5, 1, 1, 0, 0, 0, 1, 0, 1, '/', 0),
		"even stream":         append(append([]byte{}, wire.Preface...), 0, 0, 5, 1, 1, 0, 0, 2, 1, 0, 1, '/', 0),
		"response to server":  append(append([]byte{}, wire.Preface...), 0, 0, 3, 2, 1, 0, 0, 1, 0, 200, 0),
		"orphan DATA":         append(append([]byte{}, wire.Preface...), 0, 0, 2, 3, 1, 0, 0, 9, 'h', 'i'),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			p := dial(t, s, false)
			p.write(raw)
			expectErrorThenClose(t, p)
		})
	}
	if s.Stats().ConnErrors != int64(len(cases)) {
		t.Errorf("%d connection errors counted", s.Stats().ConnErrors)
	}
}

func TestReusedStreamIDIsFatalEvenAfterAGoodResponse(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	p := dial(t, s, true)
	p.request("GET", "/hello.txt", 5)
	p.response(5, true)
	p.request("GET", "/hello.txt", 3)
	expectErrorThenClose(t, p)
}

func TestErrorFromThePeerEndsTheConnectionSilently(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	p := dial(t, s, true)
	p.write(wire.EncodeError(400, "client giving up").Encode())
	p.expectClosed()
}

func TestRequestCapAnnouncesConnectionClose(t *testing.T) {
	s := startServer(t, docRoot(t), func(c *Config) { c.MaxRequestsPerConn = 2 })
	p := dial(t, s, true)
	p.request("GET", "/hello.txt", 1)
	if resp, _ := p.response(1, true); header(t, resp, "connection") != "" {
		t.Fatal("first response already says connection: close")
	}
	p.request("GET", "/hello.txt", 3)
	if resp, _ := p.response(3, true); header(t, resp, "connection") != "close" {
		t.Fatal("SPEC 1.4: a server closing after a response must say so in it")
	}
	p.expectClosed()
}

func TestIdleConnectionIsClosed(t *testing.T) {
	s := startServer(t, docRoot(t), func(c *Config) { c.IdleTimeout = 150 * time.Millisecond })
	p := dial(t, s, true)
	start := time.Now()
	p.expectClosed()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("closed after %v", d)
	}
}

func TestShutdownClosesIdleConnectionsPromptly(t *testing.T) {
	s, err := New(Config{Root: docRoot(t), Addr: "127.0.0.1:0", IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve() }()
	p := dial(t, s, true)
	p.request("GET", "/hello.txt", 1)
	p.response(1, true)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	p.expectClosed()
	if _, err := net.DialTimeout("tcp", s.Addr(), 200*time.Millisecond); err == nil {
		t.Fatal("still accepting after shutdown")
	}
}

func TestManyConcurrentConnections(t *testing.T) {
	s := startServer(t, docRoot(t), nil)
	const n = 32
	errs := make(chan error, n)
	for i := range n {
		go func() {
			nc, err := net.Dial("tcp", s.Addr())
			if err != nil {
				errs <- err
				return
			}
			defer nc.Close()
			frames, _ := wire.EncodeRequest(&wire.Request{Method: "GET", Path: "/docs/nested.txt"}, 1, 0)
			if _, err := nc.Write(frames[0].AppendTo(append([]byte{}, wire.Preface...))); err != nil {
				errs <- err
				return
			}
			nc.SetReadDeadline(time.Now().Add(3 * time.Second))
			r := wire.NewReader(false)
			a := wire.NewAssembler(wire.RoleClient)
			a.OpenStream(1, true)
			buf := make([]byte, 4096)
			for {
				n, err := nc.Read(buf)
				r.Feed(buf[:n])
				for {
					f, ok, ferr := r.Next()
					if ferr != nil || !ok {
						break
					}
					if m, aerr := a.Accept(f); aerr != nil {
						errs <- aerr
						return
					} else if m != nil {
						errs <- nil
						return
					}
				}
				if err != nil {
					errs <- errors.New("conn " + strconv.Itoa(i) + ": " + err.Error())
					return
				}
			}
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}
