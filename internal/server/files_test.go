package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func docRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<h1>home</h1>")
	write("hello.txt", "Hello, world!\n")
	write("docs/nested.txt", "nested")
	write("docs/index.html", "docs index")
	write("a b.txt", "space")
	write("%2e%2e", "a file literally named %2e%2e")
	write("empty.bin", "")
	if err := os.Mkdir(filepath.Join(root, "noindex"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestResolve(t *testing.T) {
	root := docRoot(t)
	files, err := NewFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	ok := map[string]string{
		"/":                  "index.html",
		"/index.html":        "index.html",
		"/docs":              "docs/index.html",
		"/docs/":             "docs/index.html",
		"/docs/nested.txt":   "docs/nested.txt",
		"//docs//nested.txt": "docs/nested.txt",
		"/a%20b.txt":         "a b.txt",
		"/hello.txt?x=1":     "hello.txt",
		"/%252e%252e":        "%2e%2e",
	}
	for target, want := range ok {
		got, err := files.Resolve(target)
		if err != nil {
			t.Errorf("Resolve(%q): %v", target, err)
			continue
		}
		if wantPath := filepath.Join(files.Root(), filepath.FromSlash(want)); !samePath(got, wantPath) {
			t.Errorf("Resolve(%q) = %s, want %s", target, got, wantPath)
		}
	}

	refused := []string{
		"/../secret", "/docs/../../secret", "/%2e%2e/secret", "/%2E%2E/secret",
		"/docs/%2e%2e/%2e%2e/secret", "/a%2f..%2f..%2fsecret", "/a\\b", "/a%5cb",
		"/%00", "/a%0ab", "/C:/Windows/win.ini", "/c:", "/hello.txt:$DATA",
		"/hello.txt::$DATA", "/con", "/CON", "/nul.txt", "/lpt1", "/com9.log",
		"/aux.tar.gz", "/conin$", "/docs/prn", "/hello.txt.", "/hello.txt ",
		"/%", "/%zz",
	}
	for _, target := range refused {
		if _, err := files.Resolve(target); !errors.Is(err, ErrBadPath) {
			t.Errorf("Resolve(%q) = %v, want ErrBadPath", target, err)
		}
	}

	for _, target := range []string{"/missing", "/docs/missing.txt", "/noindex", "/hello.txt/x"} {
		if _, err := files.Resolve(target); !errors.Is(err, ErrNotFound) {
			t.Errorf("Resolve(%q) = %v, want ErrNotFound", target, err)
		}
	}
}

func TestSymlinkOutOfTheRootIsRefused(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := docRoot(t)
	if err := os.Symlink(secret, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("cannot create directory symlinks here: %v", err)
	}
	files, err := NewFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/link.txt", "/linkdir/secret.txt", "/linkdir"} {
		if _, err := files.Resolve(target); !errors.Is(err, ErrBadPath) && !errors.Is(err, ErrNotFound) {
			t.Errorf("Resolve(%q) = %v, want refusal", target, err)
		} else if err == nil {
			t.Errorf("Resolve(%q) escaped the root", target)
		}
	}
}

func TestRefusalsDoNotLeakHostPaths(t *testing.T) {
	root := docRoot(t)
	files, _ := NewFiles(root)
	for _, target := range []string{"/missing", "/../x", "/noindex"} {
		_, err := files.Resolve(target)
		if err != nil && containsFold(err.Error(), files.Root()) {
			t.Errorf("error for %q reveals the root: %v", target, err)
		}
	}
}

func TestNewFilesRejectsAFile(t *testing.T) {
	root := docRoot(t)
	if _, err := NewFiles(filepath.Join(root, "hello.txt")); err == nil {
		t.Fatal("a regular file was accepted as a document root")
	}
	if _, err := NewFiles(filepath.Join(root, "nope")); err == nil {
		t.Fatal("a missing directory was accepted as a document root")
	}
}

func TestContentType(t *testing.T) {
	for name, want := range map[string]string{
		"index.html": "text/html", "A.TXT": "text/plain", "x.png": "image/png",
		"noext": "application/octet-stream", "x.unknown": "application/octet-stream",
	} {
		if got := contentType(name); got != want {
			t.Errorf("contentType(%q) = %q, want %q", name, got, want)
		}
	}
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
