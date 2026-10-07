package client

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bht1/internal/server"
	"bht1/internal/wire"
)

func TestParseURL(t *testing.T) {
	ok := map[string]Target{
		"localhost:9000/index.html": {"localhost", 9000, "/index.html"},
		"bht://localhost:9000/a/b":  {"localhost", 9000, "/a/b"},
		"localhost:9000":            {"localhost", 9000, "/"},
		"example.org/x":             {"example.org", 9000, "/x"},
		"[::1]:9001/x":              {"::1", 9001, "/x"},
		"[::1]/x":                   {"::1", 9000, "/x"},
		"h:1/a%20b?q=1":             {"h", 1, "/a%20b?q=1"},
	}
	for in, want := range ok {
		got, err := ParseURL(in)
		if err != nil || got != want {
			t.Errorf("ParseURL(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{
		"localhost:http/x", "localhost:0/x", "localhost:70000/x", "localhost:+1/x",
		":9000/x", "/x", "::1/x", "[::1/x", "http://h/x", "h/../etc", "h/a b",
	} {
		if got, err := ParseURL(in); err == nil {
			t.Errorf("ParseURL(%q) = %+v, want error", in, got)
		}
	}
}

func www(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"index.html": "<h1>home</h1>", "hello.txt": "Hello, world!\n", "docs/nested.txt": "nested",
	} {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func realServer(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.New(server.Config{Root: www(t), Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return s
}

func targets(t *testing.T, addr string, paths ...string) []Target {
	t.Helper()
	var out []Target
	for _, p := range paths {
		tg, err := ParseURL(addr + p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tg)
	}
	return out
}

func TestThreeURLsShareOneConnection(t *testing.T) {
	s := realServer(t)
	res, err := Fetch(context.Background(), targets(t, s.Addr(), "/index.html", "/hello.txt", "/docs/nested.txt"), "GET", Options{})
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for i, ex := range res.Exchanges {
		if ex.StreamID != uint16(1+2*i) {
			t.Errorf("exchange %d on stream %d", i, ex.StreamID)
		}
		bodies = append(bodies, string(ex.Response.Body))
	}
	if strings.Join(bodies, "|") != "<h1>home</h1>|Hello, world!\n|nested" {
		t.Fatalf("bodies %q", bodies)
	}
	if res.Connections != 1 || s.Stats().Accepted != 1 {
		t.Fatalf("connections: client %d, server %d", res.Connections, s.Stats().Accepted)
	}
	if ExitCode(res, err) != ExitOK {
		t.Fatal(ExitCode(res, err))
	}
}

func TestExitCodes(t *testing.T) {
	s := realServer(t)
	res, err := Fetch(context.Background(), targets(t, s.Addr(), "/hello.txt", "/missing"), "GET", Options{})
	if got := ExitCode(res, err); got != ExitClientError {
		t.Errorf("404: exit %d", got)
	}
	res, err = Fetch(context.Background(), targets(t, s.Addr(), "/hello.txt"), "HEAD", Options{})
	if got := ExitCode(res, err); got != ExitOK || len(res.Exchanges[0].Response.Body) != 0 {
		t.Errorf("HEAD: exit %d", got)
	}

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	res, err = Fetch(context.Background(), targets(t, addr, "/x"), "GET", Options{Timeout: time.Second})
	if got := ExitCode(res, err); got != ExitConnect {
		t.Errorf("refused: exit %d (%v)", got, err)
	}

	server5xx := fake(t, 1, func(ids []uint16) []byte {
		return mustResponse(t, &wire.Response{Status: 503}, ids[0])
	})
	res, err = Fetch(context.Background(), targets(t, server5xx, "/x"), "GET", Options{})
	if got := ExitCode(res, err); got != ExitServerError {
		t.Errorf("503: exit %d", got)
	}
}

func fake(t *testing.T, n int, reply func(ids []uint16) []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		defer nc.Close()
		r := wire.NewReader(true)
		a := wire.NewAssembler(wire.RoleServer)
		var ids []uint16
		buf := make([]byte, 4096)
		for len(ids) < n {
			f, ok, err := r.Next()
			if err != nil {
				return
			}
			if !ok {
				k, err := nc.Read(buf)
				r.Feed(buf[:k])
				if err != nil && k == 0 {
					return
				}
				continue
			}
			if m, _ := a.Accept(f); m != nil {
				ids = append(ids, m.Stream())
			}
		}
		nc.Write(reply(ids))
		nc.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			if _, err := nc.Read(buf); err != nil {
				return
			}
		}
	}()
	return ln.Addr().String()
}

func mustResponse(t *testing.T, r *wire.Response, id uint16) []byte {
	t.Helper()
	frames, err := wire.EncodeResponse(r, id, 4)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, f := range frames {
		out = f.AppendTo(out)
	}
	return out
}

func TestAServerFromTheFuture(t *testing.T) {
	addr := fake(t, 2, func(ids []uint16) []byte {
		var out []byte
		out = wire.Frame{Type: 0x05, Payload: []byte("SETTINGS v2")}.AppendTo(out)

		out = append(out, mustResponse(t, &wire.Response{Status: 200, Body: []byte("second")}, ids[1])...)
		first := mustResponse(t, &wire.Response{Status: 200, Body: []byte("first body")}, ids[0])
		headEnd := wire.HeaderSize + 3
		out = append(out, first[:headEnd]...)
		out = wire.Frame{Type: 0xee, StreamID: ids[0], Flags: 0xff, Payload: bytes.Repeat([]byte{7}, 5000)}.AppendTo(out)
		return append(out, first[headEnd:]...)
	})
	res, err := Fetch(context.Background(), targets(t, addr, "/a", "/b"), "GET", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Exchanges[0].Response.Body) != "first body" || string(res.Exchanges[1].Response.Body) != "second" {
		t.Fatalf("%q %q", res.Exchanges[0].Response.Body, res.Exchanges[1].Response.Body)
	}
}

func TestConnectionErrorFrameIsExit8(t *testing.T) {
	addr := fake(t, 1, func([]uint16) []byte { return wire.EncodeError(400, "bad preface").Encode() })
	res, err := Fetch(context.Background(), targets(t, addr, "/x"), "GET", Options{})
	if got := ExitCode(res, err); got != ExitProtocol || len(res.Errors) != 1 || res.Errors[0].Reason != "bad preface" {
		t.Fatalf("exit %d, errors %+v, err %v", got, res.Errors, err)
	}
}

func TestHangUpBeforeTheResponseIsExit8(t *testing.T) {
	addr := fake(t, 1, func([]uint16) []byte { return nil })
	res, err := Fetch(context.Background(), targets(t, addr, "/x"), "GET", Options{Timeout: time.Second})
	if got := ExitCode(res, err); got != ExitProtocol {
		t.Fatalf("exit %d (%v)", got, err)
	}
}

func TestMalformedResponseFailsOnlyItsStream(t *testing.T) {
	addr := fake(t, 2, func(ids []uint16) []byte {
		bad := mustResponse(t, &wire.Response{Status: 200, Headers: wire.Headers{{Name: "content-length", Value: "99"}}, Body: []byte("short")}, ids[0])
		return append(bad, mustResponse(t, &wire.Response{Status: 200, Body: []byte("fine")}, ids[1])...)
	})
	res, err := Fetch(context.Background(), targets(t, addr, "/a", "/b"), "GET", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Exchanges[0].Err == nil || string(res.Exchanges[1].Response.Body) != "fine" {
		t.Fatalf("%+v %+v", res.Exchanges[0], res.Exchanges[1])
	}
	if ExitCode(res, err) != ExitProtocol {
		t.Fatal("a malformed response must not exit 0")
	}
}

func TestVerboseTraceNamesEveryField(t *testing.T) {
	s := realServer(t)
	var trace bytes.Buffer
	if _, err := Fetch(context.Background(), targets(t, s.Addr(), "/hello.txt"), "GET", Options{Trace: &trace}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`> 0000  42 48 54 31`, `Connection preface "BHT1"`, "Type = 0x01 REQUEST",
		"Flags = 0x01 END_MESSAGE", "Reserved, MUST be ignored on receipt", "Stream ID = 1",
		`Path = "/hello.txt"`, `Static index 3 -> "host"`, "< 0000", "Status = 200",
		"Body bytes (14)", "* 1 TCP connection, 1 frames out, 2 frames in",
	} {
		if !strings.Contains(trace.String(), want) {
			t.Errorf("trace lacks %q", want)
		}
	}
}

func TestRefusesWhatOneConnectionCannotCarry(t *testing.T) {
	many := make([]Target, MaxRequests+1)
	for i := range many {
		many[i] = Target{"h", 1, "/"}
	}
	if _, err := Fetch(context.Background(), many, "GET", Options{}); err == nil {
		t.Fatal("accepted more requests than odd stream IDs")
	}
	mixed := []Target{{"a", 1, "/"}, {"b", 1, "/"}}
	if _, err := Fetch(context.Background(), mixed, "GET", Options{}); err == nil {
		t.Fatal("accepted two authorities")
	}
}

func TestCancellationStopsAWaitingFetch(t *testing.T) {
	addr := fake(t, 1, func([]uint16) []byte { time.Sleep(3 * time.Second); return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Fetch(ctx, targets(t, addr, "/x"), "GET", Options{})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}
