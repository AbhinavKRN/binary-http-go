package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bht1/internal/wire"
)

var (
	ErrBadPath = errors.New("path refused")

	ErrNotFound = errors.New("no such file")
)

const DefaultIndex = "index.html"

var windowsDevices = func() map[string]bool {
	m := map[string]bool{"con": true, "prn": true, "aux": true, "nul": true, "conin$": true, "conout$": true}
	for _, d := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
		m["com"+d], m["lpt"+d] = true, true
	}
	return m
}()

type Files struct {
	root  string
	index string
}

func NewFiles(root string) (*Files, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", real)
	}
	return &Files{root: real, index: DefaultIndex}, nil
}

func (f *Files) Root() string { return f.root }

func (f *Files) Resolve(target string) (string, error) {
	decoded, err := wire.DecodePath(target)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadPath, err)
	}
	var segments []string
	for seg := range strings.SplitSeq(decoded, "/") {
		if seg == "" {
			continue
		}
		if err := checkSegment(seg); err != nil {
			return "", fmt.Errorf("%w: %v", ErrBadPath, err)
		}
		segments = append(segments, seg)
	}

	real, err := f.contain(filepath.Join(append([]string{f.root}, segments...)...))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNotFound, target)
	}
	if info.IsDir() {

		if real, err = f.contain(filepath.Join(real, f.index)); err != nil {
			return "", err
		}
		if info, err = os.Stat(real); err != nil {
			return "", fmt.Errorf("%w: %s", ErrNotFound, target)
		}
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s", ErrNotFound, target)
	}
	return real, nil
}

func (f *Files) contain(candidate string) (string, error) {

	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", ErrNotFound
	}
	rel, err := filepath.Rel(f.root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: resolves outside the document root", ErrBadPath)
	}
	return real, nil
}

func checkSegment(seg string) error {
	if strings.ContainsAny(seg, `:\`) {
		return fmt.Errorf("drive, stream or separator character in %q", seg)
	}

	if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
		return fmt.Errorf("trailing dot or space in %q", seg)
	}
	base, _, _ := strings.Cut(seg, ".")
	if windowsDevices[strings.ToLower(strings.TrimRight(base, " "))] {
		return fmt.Errorf("reserved device name %q", seg)
	}
	return nil
}

var contentTypes = map[string]string{
	".html": "text/html",
	".htm":  "text/html",
	".txt":  "text/plain",
	".css":  "text/css",
	".js":   "text/javascript",
	".mjs":  "text/javascript",
	".json": "application/json",
	".xml":  "application/xml",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".ico":  "image/vnd.microsoft.icon",
	".pdf":  "application/pdf",
	".wasm": "application/wasm",
	".md":   "text/markdown",
	".csv":  "text/csv",
}

func contentType(name string) string {
	if t, ok := contentTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}
