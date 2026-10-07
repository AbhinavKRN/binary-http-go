package wire

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var vectorDir = filepath.Join("..", "..", "spec", "vectors")

type vector struct {
	Name           string     `json:"name"`
	Role           string     `json:"role"`
	Preface        *bool      `json:"preface"`
	Canonical      bool       `json:"canonical"`
	MaxData        int        `json:"max_data"`
	OpenStreams    []uint16   `json:"open_streams"`
	SkippedUnknown int        `json:"skipped_unknown"`
	Hex            string     `json:"hex"`
	Expect         []expected `json:"expect"`
	Reject         string     `json:"reject"`
	Status         int        `json:"status"`
}

type expected struct {
	Kind     string      `json:"kind"`
	StreamID uint16      `json:"stream_id"`
	Method   string      `json:"method"`
	Path     string      `json:"path"`
	Status   int         `json:"status"`
	Reason   string      `json:"reason"`
	Headers  [][2]string `json:"headers"`
	Body     string      `json:"body"`
}

func loadVectors(t testing.TB, name string) []vector {
	t.Helper()
	f, err := os.Open(filepath.Join(vectorDir, name))
	if err != nil {
		t.Fatalf("conformance vectors not found: %v", err)
	}
	defer f.Close()
	var out []vector
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v vector
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no vectors", name)
	}
	return out
}

func unhexVector(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

func latin1(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		out = append(out, byte(r))
	}
	return out
}

func drive(v vector, data []byte, chunk int) ([]Message, *Assembler, error) {
	role := RoleServer
	if v.Role == "client" {
		role = RoleClient
	}
	wantPreface := role == RoleServer && (v.Preface == nil || *v.Preface)
	r := NewReader(wantPreface)
	a := NewAssembler(role)
	for _, id := range v.OpenStreams {
		a.OpenStream(id, true)
	}
	wire := data
	if wantPreface {
		wire = append(append([]byte{}, Preface...), data...)
	}
	if chunk <= 0 {
		chunk = len(wire)
	}
	var msgs []Message
	for start := 0; start < len(wire); start += chunk {
		r.Feed(wire[start:min(start+chunk, len(wire))])
		for {
			f, ok, err := r.Next()
			if err != nil {
				return msgs, a, err
			}
			if !ok {
				break
			}
			m, err := a.Accept(f)
			if err != nil {
				return msgs, a, err
			}
			if m != nil {
				msgs = append(msgs, m)
			}
		}
	}
	return msgs, a, nil
}

func headerPairs(h Headers) [][2]string {
	out := make([][2]string, 0, len(h))
	for _, e := range h {
		out = append(out, [2]string{e.Name, e.Value})
	}
	return out
}

func checkMessage(t *testing.T, got Message, want expected) {
	t.Helper()
	if got.Stream() != want.StreamID {
		t.Errorf("stream id = %d, want %d", got.Stream(), want.StreamID)
	}
	equalHeaders := func(h Headers) {
		gotPairs, wantPairs := headerPairs(h), want.Headers
		if wantPairs == nil {
			wantPairs = [][2]string{}
		}
		if len(gotPairs) != len(wantPairs) {
			t.Errorf("headers = %v, want %v", gotPairs, wantPairs)
			return
		}
		for i := range gotPairs {
			if gotPairs[i] != wantPairs[i] {
				t.Errorf("headers = %v, want %v", gotPairs, wantPairs)
				return
			}
		}
	}
	switch want.Kind {
	case "request":
		m, ok := got.(*Request)
		if !ok {
			t.Fatalf("got %T, want *Request", got)
		}
		if m.Method != want.Method || m.Path != want.Path {
			t.Errorf("request line = %s %s, want %s %s", m.Method, m.Path, want.Method, want.Path)
		}
		equalHeaders(m.Headers)
		if !bytes.Equal(m.Body, latin1(want.Body)) {
			t.Errorf("body = %q, want %q", m.Body, want.Body)
		}
	case "response":
		m, ok := got.(*Response)
		if !ok {
			t.Fatalf("got %T, want *Response", got)
		}
		if m.Status != want.Status {
			t.Errorf("status = %d, want %d", m.Status, want.Status)
		}
		equalHeaders(m.Headers)
		if !bytes.Equal(m.Body, latin1(want.Body)) {
			t.Errorf("body = %q, want %q", m.Body, want.Body)
		}
	case "error":
		m, ok := got.(*ErrorMsg)
		if !ok {
			t.Fatalf("got %T, want *ErrorMsg", got)
		}
		if m.Status != want.Status || m.Reason != want.Reason {
			t.Errorf("error = %d %q, want %d %q", m.Status, m.Reason, want.Status, want.Reason)
		}
	default:
		t.Fatalf("unknown kind %q in vector", want.Kind)
	}
}

func TestValidVectors(t *testing.T) {
	for _, file := range []string{"requests.jsonl", "responses.jsonl"} {
		for _, v := range loadVectors(t, file) {
			data := unhexVector(t, v.Hex)
			for _, mode := range []struct {
				name  string
				chunk int
			}{{"whole", 0}, {"byte-at-a-time", 1}} {
				t.Run(file+"/"+v.Name+"/"+mode.name, func(t *testing.T) {
					msgs, a, err := drive(v, data, mode.chunk)
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					if len(msgs) != len(v.Expect) {
						t.Fatalf("%d messages, want %d", len(msgs), len(v.Expect))
					}
					for i := range msgs {
						checkMessage(t, msgs[i], v.Expect[i])
					}
					if a.SkippedUnknown != v.SkippedUnknown {
						t.Errorf("skipped %d unknown frames, want %d", a.SkippedUnknown, v.SkippedUnknown)
					}
				})
			}
		}
	}
}

func TestCanonicalVectorsReencode(t *testing.T) {
	for _, file := range []string{"requests.jsonl", "responses.jsonl"} {
		for _, v := range loadVectors(t, file) {
			if !v.Canonical {
				continue
			}
			t.Run(file+"/"+v.Name, func(t *testing.T) {
				want := v.Expect[0]
				var headers Headers
				for _, h := range want.Headers {
					headers = append(headers, Header{Name: h[0], Value: h[1]})
				}
				var frames []Frame
				var err error
				switch want.Kind {
				case "request":
					frames, err = EncodeRequest(&Request{Method: want.Method, Path: want.Path, Headers: headers, Body: latin1(want.Body)}, want.StreamID, v.MaxData)
				case "response":
					frames, err = EncodeResponse(&Response{Status: want.Status, Headers: headers, Body: latin1(want.Body)}, want.StreamID, v.MaxData)
				}
				if err != nil {
					t.Fatal(err)
				}
				var got []byte
				for _, f := range frames {
					got = f.AppendTo(got)
				}
				if wantBytes := unhexVector(t, v.Hex); !bytes.Equal(got, wantBytes) {
					t.Errorf("encoded\n  % x\nwant\n  % x", got, wantBytes)
				}
			})
		}
	}
}

func TestInvalidVectorsAreRejected(t *testing.T) {
	for _, v := range loadVectors(t, "invalid.jsonl") {
		data := unhexVector(t, v.Hex)
		for _, chunk := range []int{0, 1} {
			mode := map[int]string{0: "whole", 1: "byte-at-a-time"}[chunk]
			t.Run(v.Name+"/"+mode, func(t *testing.T) {
				_, _, err := drive(v, data, chunk)
				var se *StreamError
				var ce *ConnError
				switch {
				case err == nil:
					t.Fatalf("accepted; want %s-level %d", v.Reject, v.Status)
				case v.Reject == "connection" && errors.As(err, &ce):
					if ce.Status != v.Status {
						t.Errorf("status %d, want %d", ce.Status, v.Status)
					}
				case v.Reject == "stream" && errors.As(err, &se):
					if se.Status != v.Status {
						t.Errorf("status %d, want %d", se.Status, v.Status)
					}
				default:
					t.Errorf("got %T (%v), want a %s-level rejection", err, err, v.Reject)
				}
			})
		}
	}
}

func TestStreamFailureLeavesConnectionUsable(t *testing.T) {
	bad := unhexVector(t, "00 00 05 01 01 00 00 01  00 00 01 2f 00")
	good := unhexVector(t, "00 00 09 01 01 00 00 03  01 00 01 2f 01 83 00 01 78")
	r := NewReader(true)
	a := NewAssembler(RoleServer)
	r.Feed(Preface)
	r.Feed(bad)
	r.Feed(good)

	f, _, _ := r.Next()
	var se *StreamError
	if _, err := a.Accept(f); !errors.As(err, &se) || se.StreamID != 1 {
		t.Fatalf("want stream error on stream 1, got %v", err)
	}
	f, _, _ = r.Next()
	m, err := a.Accept(f)
	if err != nil {
		t.Fatal(err)
	}
	if req, ok := m.(*Request); !ok || req.StreamID != 3 {
		t.Fatalf("got %#v, want request on stream 3", m)
	}
}
