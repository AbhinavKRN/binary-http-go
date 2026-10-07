package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageErrorsExit2(t *testing.T) {
	root := t.TempDir()
	cases := map[string][]string{
		"no root":      {},
		"missing root": {filepath.Join(root, "nope")},
		"bad port":     {root, "http"},
		"port range":   {root, "70000"},
		"extra arg":    {root, "9000", "more"},
		"unknown flag": {root, "--nope"},
	}
	for name, args := range cases {
		var stderr bytes.Buffer
		if code := run(args, &stderr); code != 2 {
			t.Errorf("%s: exit %d, want 2 (%s)", name, code, strings.TrimSpace(stderr.String()))
		}
	}
}
