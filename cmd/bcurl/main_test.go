package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bht1/internal/server"
)

func serve(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "hello.txt"), []byte("Hello, world!\n"), 0o644)
	os.WriteFile(filepath.Join(root, "index.html"), []byte("<h1>home</h1>"), 0o644)
	s, err := server.New(server.Config{Root: root, Addr: "127.0.0.1:0"})
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
	return s.Addr()
}

func TestBodiesGoToStdoutAndDiagnosticsToStderr(t *testing.T) {
	addr := serve(t)
	var out, errOut bytes.Buffer
	code := run([]string{addr + "/hello.txt", "-v", addr + "/"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if out.String() != "Hello, world!\n<h1>home</h1>" {
		t.Fatalf("stdout %q", out.String())
	}
	if !strings.Contains(errOut.String(), "Type = 0x02 RESPONSE") {
		t.Fatal("-v after a URL was not honoured")
	}
}

func TestOutputFileAndHeaders(t *testing.T) {
	addr := serve(t)
	path := filepath.Join(t.TempDir(), "body")
	var out, errOut bytes.Buffer
	if code := run([]string{"-o", path, "-H", "Accept: text/plain", addr + "/hello.txt"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if got, _ := os.ReadFile(path); string(got) != "Hello, world!\n" || out.Len() != 0 {
		t.Fatalf("file %q, stdout %q", got, out.String())
	}
}

func TestExitStatuses(t *testing.T) {
	addr := serve(t)
	cases := []struct {
		args []string
		want int
	}{
		{[]string{addr + "/missing"}, 4},
		{[]string{"-I", addr + "/hello.txt"}, 0},
		{[]string{}, 2},
		{[]string{"-nope", addr + "/"}, 2},
		{[]string{"localhost:9000/a", "localhost:9001/b"}, 2},
		{[]string{"-H", "no-colon", addr + "/"}, 2},
		{[]string{"localhost:bad/x"}, 2},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		if got := run(c.args, &out, &errOut); got != c.want {
			t.Errorf("bcurl %v: exit %d, want %d (%s)", c.args, got, c.want, errOut.String())
		}
	}
}
