package interop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"bht1/internal/hexdump"
	"bht1/internal/server"
	"bht1/internal/wire"
)

var (
	pyImpl   = os.Getenv("BHT1_PY_IMPL")
	www      = filepath.Join("..", "www")
	vectors  = filepath.Join("..", "spec", "vectors")
	python   string
	goBcurl  string
	skipWhy  string
	buildErr error
)

func TestMain(m *testing.M) {
	python, skipWhy = findPython()
	dir, err := os.MkdirTemp("", "bht1-interop")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if skipWhy == "" {
		goBcurl = filepath.Join(dir, "bcurl")
		if runtime.GOOS == "windows" {
			goBcurl += ".exe"
		}
		out, err := exec.Command("go", "build", "-o", goBcurl, "bht1/cmd/bcurl").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("building bcurl: %v\n%s", err, out)
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func findPython() (string, string) {
	if pyImpl == "" {
		return "", "set BHT1_PY_IMPL to the Python implementation's directory to run interop tests"
	}
	if abs, err := filepath.Abs(pyImpl); err == nil {
		pyImpl = abs
	}
	if _, err := os.Stat(filepath.Join(pyImpl, "bserve.py")); err != nil {
		return "", "BHT1_PY_IMPL has no bserve.py: " + pyImpl
	}
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if exec.Command(path, "-c", "import sys; sys.exit(sys.version_info < (3, 11))").Run() == nil {
			return path, ""
		}
	}
	return "", "Python 3.11+ is not on PATH"
}

func need(t *testing.T) {
	t.Helper()
	if skipWhy != "" {
		t.Skip(skipWhy)
	}
	if buildErr != nil {
		t.Fatal(buildErr)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func pythonServer(t *testing.T) (string, *syncBuffer) {
	t.Helper()
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, python, filepath.Join(pyImpl, "bserve.py"), www, strconv.Itoa(port))
	log := &syncBuffer{}
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(10 * time.Second)
	for {
		nc, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			nc.Close()
			return addr, log
		}
		if time.Now().After(deadline) {
			t.Fatalf("bserve.py never listened on %s:\n%s", addr, log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func goServer(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.New(server.Config{Root: www, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return s
}

type ran struct {
	code   int
	stdout []byte
	stderr string
}

func runCmd(t *testing.T, name string, args ...string) ran {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return ran{code, out.Bytes(), errOut.String()}
}

func fileBytes(t *testing.T, names ...string) []byte {
	t.Helper()
	var out []byte
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(www, filepath.FromSlash(n)))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	return out
}

var pyRequestLine = regexp.MustCompile(`(\S+:\d+) stream (\d+)\s+(\w+) (\S+) -> (\d+)`)

func TestGoClientAgainstPythonServer(t *testing.T) {
	need(t)
	addr, log := pythonServer(t)

	t.Run("three files, one connection", func(t *testing.T) {
		r := runCmd(t, goBcurl, "-v", addr+"/index.html", addr+"/hello.txt", addr+"/docs/nested.txt")
		if r.code != 0 {
			t.Fatalf("exit %d\n%s", r.code, r.stderr)
		}
		if want := fileBytes(t, "index.html", "hello.txt", "docs/nested.txt"); !bytes.Equal(r.stdout, want) {
			t.Fatalf("bodies differ:\n got %q\nwant %q", r.stdout, want)
		}
		if !strings.Contains(r.stderr, "* 1 TCP connection") {
			t.Error("bcurl does not report a single connection")
		}

		time.Sleep(100 * time.Millisecond)
		peers := map[string]bool{}
		for _, m := range pyRequestLine.FindAllStringSubmatch(log.String(), -1) {
			peers[m[1]] = true
		}
		if len(peers) != 1 {
			t.Errorf("bserve.py saw requests from %d peers, want 1:\n%s", len(peers), log.String())
		}
	})

	t.Run("404 is exit 4", func(t *testing.T) {
		if r := runCmd(t, goBcurl, addr+"/missing.txt"); r.code != 4 {
			t.Fatalf("exit %d\n%s", r.code, r.stderr)
		}
	})

	t.Run("HEAD has no body", func(t *testing.T) {
		r := runCmd(t, goBcurl, "-I", addr+"/hello.txt")
		if r.code != 0 || len(r.stdout) != 0 {
			t.Fatalf("exit %d, %d body bytes\n%s", r.code, len(r.stdout), r.stderr)
		}
	})

	t.Run("HEAD of a missing file", func(t *testing.T) {
		r := runCmd(t, goBcurl, "-I", addr+"/missing.txt")
		if r.code == 8 && strings.Contains(r.stderr, "DATA in the response to a HEAD request") {
			t.Skip("known divergence, SPEC-NOTES.md finding 1: bserve.py sends DATA in a HEAD 404 (SPEC 5.6)")
		}
		if r.code != 4 || len(r.stdout) != 0 {
			t.Fatalf("exit %d, %d body bytes\n%s", r.code, len(r.stdout), r.stderr)
		}
	})

	t.Run("percent-encoded path", func(t *testing.T) {
		r := runCmd(t, goBcurl, addr+"/docs/%6eested.txt")
		if r.code != 0 || !bytes.Equal(r.stdout, fileBytes(t, "docs/nested.txt")) {
			t.Fatalf("exit %d\n%s", r.code, r.stderr)
		}
	})
}

func TestPythonClientAgainstGoServer(t *testing.T) {
	need(t)
	s := goServer(t)
	addr := s.Addr()
	bcurlPy := filepath.Join(pyImpl, "bcurl.py")
	out := filepath.Join(t.TempDir(), "body")

	t.Run("three files, one connection", func(t *testing.T) {
		before := s.Stats().Accepted
		r := runCmd(t, python, bcurlPy, "-o", out, addr+"/index.html", addr+"/hello.txt", addr+"/docs/nested.txt")
		if r.code != 0 {
			t.Fatalf("exit %d\n%s", r.code, r.stderr)
		}
		got, _ := os.ReadFile(out)
		if want := fileBytes(t, "index.html", "hello.txt", "docs/nested.txt"); !bytes.Equal(got, want) {
			t.Fatalf("bodies differ:\n got %q\nwant %q", got, want)
		}
		if n := s.Stats().Accepted - before; n != 1 {
			t.Errorf("bcurl.py opened %d connections, want 1", n)
		}
	})

	t.Run("verbose decode of Go frames", func(t *testing.T) {
		r := runCmd(t, python, bcurlPy, "-v", "-o", out, addr+"/hello.txt")
		if r.code != 0 {
			t.Fatalf("exit %d\n%s", r.code, r.stderr)
		}
		for _, want := range []string{"Type = 0x02 RESPONSE", "Status = 200", `Static index 5 -> "server"`, "Body bytes"} {
			if !strings.Contains(r.stderr, want) {
				t.Errorf("bcurl.py -v lacks %q", want)
			}
		}
	})

	t.Run("404 is exit 4", func(t *testing.T) {
		if r := runCmd(t, python, bcurlPy, "-o", out, addr+"/missing.txt"); r.code != 4 {
			t.Fatalf("exit %d\n%s", r.code, r.stderr)
		}
	})

	for _, path := range []string{"/hello.txt", "/missing.txt"} {
		t.Run("HEAD "+path, func(t *testing.T) {
			want := 0
			if path == "/missing.txt" {
				want = 4
			}
			r := runCmd(t, python, bcurlPy, "-I", "-o", out, addr+path)
			if got, _ := os.ReadFile(out); r.code != want || len(got) != 0 {
				t.Fatalf("exit %d, %d body bytes\n%s", r.code, len(got), r.stderr)
			}
		})
	}
}

func TestBothAnnotatorsAgree(t *testing.T) {
	need(t)
	req, _ := wire.EncodeRequest(&wire.Request{Method: "GET", Path: "/index.html", Headers: wire.Headers{
		{Name: "host", Value: "localhost:9000"}, {Name: "user-agent", Value: "bcurl/1.0"}, {Name: "x-trace", Value: "abc"},
	}}, 1, 0)
	resp, _ := wire.EncodeResponse(&wire.Response{Status: 404, Headers: wire.Headers{
		{Name: "content-type", Value: "text/plain"}, {Name: "content-length", Value: "20"},
	}, Body: []byte("not here, try again.")}, 1, 8)
	inputs := map[string][]byte{"request": append([]byte{}, wire.Preface...), "response": nil}
	for _, f := range req {
		inputs["request"] = f.AppendTo(inputs["request"])
	}
	for _, f := range resp {
		inputs["response"] = f.AppendTo(inputs["response"])
	}
	inputs["unknown and error"] = wire.EncodeError(400, "bad preface").AppendTo(
		wire.Frame{Type: 0x9a, Flags: 0x03, StreamID: 7, Payload: []byte{1, 2, 3}}.Encode())

	for name, raw := range inputs {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(python, filepath.Join(pyImpl, "tools", "hexdump.py"))
			cmd.Stdin = bytes.NewReader(raw)
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			py := strings.TrimRight(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
			goText := hexdump.Classic(raw, 0) + "\n\n" + hexdump.Render(hexdump.Annotate(raw, 0), 16)
			if py != goText {
				t.Errorf("annotators disagree\n--- python\n%s\n--- go\n%s", py, goText)
			}
		})
	}
}

type vector struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Preface *bool  `json:"preface"`
	Hex     string `json:"hex"`
	Reject  string `json:"reject"`
	Status  int    `json:"status"`
}

func invalidVectors(t *testing.T) []vector {
	t.Helper()
	f, err := os.Open(filepath.Join(vectors, "invalid.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []vector
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			var v vector
			if err := json.Unmarshal([]byte(line), &v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
	}
	return out
}

type outcome struct {
	Statuses    []int
	ConnError   int
	StillOpen   bool
	ClosedEarly bool
}

const probeStream = 0xfff1

func probe(t *testing.T, addr string, v vector) outcome {
	t.Helper()
	nc, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	raw, err := hex.DecodeString(strings.Join(strings.Fields(v.Hex), ""))
	if err != nil {
		t.Fatal(err)
	}
	if v.Preface == nil || *v.Preface {
		raw = append(append([]byte{}, wire.Preface...), raw...)
	}
	follow, _ := wire.EncodeRequest(&wire.Request{Method: "GET", Path: "/hello.txt"}, probeStream, 0)
	raw = follow[0].AppendTo(raw)
	nc.Write(raw)

	var o outcome
	r := wire.NewReader(false)
	buf := make([]byte, 64<<10)
	nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		f, ok, err := r.Next()
		if err != nil {
			t.Fatalf("server sent unparseable frames: %v", err)
		}
		if !ok {
			n, err := nc.Read(buf)
			r.Feed(buf[:n])
			if err != nil && n == 0 {
				o.ClosedEarly = errors.Is(err, io.EOF) || !isTimeout(err)
				return o
			}
			continue
		}
		switch f.Type {
		case wire.TypeError:
			o.ConnError = int(f.Payload[0])<<8 | int(f.Payload[1])
		case wire.TypeResponse:
			status := int(f.Payload[0])<<8 | int(f.Payload[1])
			if f.StreamID == probeStream {
				o.StillOpen = status == 200
				return o
			}
			o.Statuses = append(o.Statuses, status)
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func TestBothServersRejectTheInvalidVectorsAlike(t *testing.T) {
	need(t)
	pyAddr, _ := pythonServer(t)
	goAddr := goServer(t).Addr()

	for _, v := range invalidVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			for _, impl := range []struct{ name, addr string }{{"python", pyAddr}, {"go", goAddr}} {
				o := probe(t, impl.addr, v)
				switch v.Reject {
				case "connection":
					if o.ConnError != v.Status || o.StillOpen {
						t.Errorf("%s server: %+v; want ERROR %d then close", impl.name, o, v.Status)
					}
				case "stream":
					last := 0
					if len(o.Statuses) > 0 {
						last = o.Statuses[len(o.Statuses)-1]
					}
					if last != v.Status || o.ConnError != 0 || !o.StillOpen {
						t.Errorf("%s server: %+v; want %d on the stream and the connection kept", impl.name, o, v.Status)
					}
				}
			}
		})
	}
}
