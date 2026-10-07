package wire

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func feedAll(t *testing.T, r *Reader, a *Assembler, data []byte) ([]Message, error) {
	t.Helper()
	r.Feed(data)
	var out []Message
	for {
		f, ok, err := r.Next()
		if err != nil || !ok {
			return out, err
		}
		m, err := a.Accept(f)
		if err != nil {
			return out, err
		}
		if m != nil {
			out = append(out, m)
		}
	}
}

func mustFrames(frames []Frame, err error) []byte {
	if err != nil {
		panic(err)
	}
	var b []byte
	for _, f := range frames {
		b = f.AppendTo(b)
	}
	return b
}

func TestHeaderRoundTrip(t *testing.T) {
	f := Frame{Type: TypeData, Flags: FlagEndMessage, StreamID: 0xbeef, Payload: []byte("xyz")}
	raw := f.Encode()
	if want := []byte{0, 0, 3, 3, 1, 0, 0xbe, 0xef, 'x', 'y', 'z'}; !bytes.Equal(raw, want) {
		t.Fatalf("encoded % x, want % x", raw, want)
	}
	n, g := ParseHeader(raw)
	if n != 3 || g.Type != TypeData || g.Flags != FlagEndMessage || g.StreamID != 0xbeef {
		t.Fatalf("parsed %d %+v", n, g)
	}
}

func TestSenderZeroesReservedByte(t *testing.T) {
	raw := Frame{Type: TypeData, Reserved: 0xff}.Encode()
	if raw[5] != 0 {
		t.Fatalf("reserved byte sent as 0x%02x", raw[5])
	}
}

func TestOversizedLengthIsRefusedOnTheHeaderAlone(t *testing.T) {
	r := NewReader(false)
	r.Feed([]byte{0x01, 0x00, 0x00, 0x03, 0, 0, 0, 1})
	_, _, err := r.Next()
	var ce *ConnError
	if !errors.As(err, &ce) || ce.Status != 400 {
		t.Fatalf("want ConnError 400, got %v", err)
	}
	if _, _, again := r.Next(); again == nil {
		t.Fatal("reader recovered from a lost frame boundary")
	}
}

func TestMaxFrameSizeExactlyIsAccepted(t *testing.T) {
	payload := make([]byte, MaxFrameSize)
	r := NewReader(false)
	r.Feed(Frame{Type: 0x42, Payload: payload}.Encode())
	f, ok, err := r.Next()
	if err != nil || !ok || len(f.Payload) != MaxFrameSize {
		t.Fatalf("ok=%v err=%v len=%d", ok, err, len(f.Payload))
	}
}

func TestPrefaceMismatchIsDetectedOnTheFirstWrongByte(t *testing.T) {
	r := NewReader(true)
	r.Feed([]byte("BH"))
	if _, ok, err := r.Next(); ok || err != nil {
		t.Fatalf("partial preface: ok=%v err=%v", ok, err)
	}
	r.Feed([]byte("X"))
	if _, _, err := r.Next(); err == nil {
		t.Fatal("bad preface accepted")
	}
}

func TestReaderBufferStaysBounded(t *testing.T) {
	r := NewReader(false)
	frame := Frame{Type: TypeData, Payload: make([]byte, 1000)}.Encode()
	for range 1000 {
		r.Feed(frame)
		if _, ok, err := r.Next(); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	if cap(r.buf) > 4*len(frame) {
		t.Fatalf("buffer grew to %d bytes for %d-byte frames", cap(r.buf), len(frame))
	}
}

func TestUnknownFrameMidBodyIsSkipped(t *testing.T) {
	var wire []byte
	wire = append(wire, Preface...)
	head, _ := EncodeRequest(&Request{Method: "POST", Path: "/u", Body: []byte("abcd")}, 1, 2)
	wire = head[0].AppendTo(wire)
	wire = head[1].AppendTo(wire)
	wire = Frame{Type: 0x7f, StreamID: 1, Payload: bytes.Repeat([]byte{0xee}, 10_000)}.AppendTo(wire)
	wire = head[2].AppendTo(wire)

	a := NewAssembler(RoleServer)
	msgs, err := feedAll(t, NewReader(true), a, wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || string(msgs[0].(*Request).Body) != "abcd" || a.SkippedUnknown != 1 {
		t.Fatalf("msgs=%v skipped=%d", msgs, a.SkippedUnknown)
	}
}

func TestContentLength(t *testing.T) {
	cases := []struct {
		values  []string
		n       int64
		present bool
		bad     bool
	}{
		{nil, 0, false, false},
		{[]string{"0"}, 0, true, false},
		{[]string{"42"}, 42, true, false},
		{[]string{"42", "42"}, 42, true, false},
		{[]string{"42", "43"}, 0, true, true},
		{[]string{"-1"}, 0, true, true},
		{[]string{"+1"}, 0, true, true},
		{[]string{" 1"}, 0, true, true},
		{[]string{""}, 0, true, true},
		{[]string{"0x10"}, 0, true, true},
		{[]string{"9999999999999999999999"}, 0, true, true},
	}
	for _, c := range cases {
		var h Headers
		for _, v := range c.values {
			h = append(h, Header{"content-length", v})
		}
		n, present, err := ContentLength(h)
		if present != c.present || (err != nil) != c.bad || (!c.bad && n != c.n) {
			t.Errorf("%v: n=%d present=%v err=%v", c.values, n, present, err)
		}
	}
}

func TestHeadResponseIsExemptFromContentLength(t *testing.T) {
	a := NewAssembler(RoleClient)
	a.OpenStream(1, false)
	raw := mustFrames(EncodeResponse(&Response{Status: 200, Headers: Headers{{"content-length", "1234"}}}, 1, 0))
	msgs, err := feedAll(t, NewReader(false), a, raw)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("msgs=%v err=%v", msgs, err)
	}
}

func TestDataInAHeadResponseIsAStreamError(t *testing.T) {
	a := NewAssembler(RoleClient)
	a.OpenStream(1, false)
	raw := mustFrames(EncodeResponse(&Response{Status: 404, Body: []byte("gone")}, 1, 0))
	_, err := feedAll(t, NewReader(false), a, raw)
	var se *StreamError
	if !errors.As(err, &se) || se.StreamID != 1 {
		t.Fatalf("want stream error, got %v", err)
	}
}

func TestResponseOnAStreamNeverRequestedIsFatal(t *testing.T) {
	a := NewAssembler(RoleClient)
	raw := mustFrames(EncodeResponse(&Response{Status: 200}, 5, 0))
	_, err := feedAll(t, NewReader(false), a, raw)
	var ce *ConnError
	if !errors.As(err, &ce) {
		t.Fatalf("want ConnError, got %v", err)
	}
}

func TestBodyOverTheCapIs413AndDrained(t *testing.T) {
	a := NewAssembler(RoleServer)
	a.MaxBody = 10
	var wire []byte
	wire = append(wire, Preface...)
	wire = append(wire, mustFrames(EncodeRequest(&Request{Method: "PUT", Path: "/x", Body: bytes.Repeat([]byte("z"), 25)}, 1, 8))...)
	wire = append(wire, mustFrames(EncodeRequest(&Request{Method: "GET", Path: "/ok"}, 3, 0))...)

	r := NewReader(true)
	r.Feed(wire)
	var statuses []int
	var got []Message
	for {
		f, ok, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		m, err := a.Accept(f)
		var se *StreamError
		if errors.As(err, &se) {
			statuses = append(statuses, se.Status)
		} else if err != nil {
			t.Fatal(err)
		}
		if m != nil {
			got = append(got, m)
		}
	}
	if len(statuses) != 1 || statuses[0] != 413 {
		t.Fatalf("statuses %v, want [413]", statuses)
	}
	if len(got) != 1 || got[0].Stream() != 3 {
		t.Fatalf("the request after the oversized one was lost: %v", got)
	}
}

func TestAnnouncedContentLengthOverTheCapFailsAtTheHead(t *testing.T) {
	a := NewAssembler(RoleServer)
	a.MaxBody = 10
	frames, _ := EncodeRequest(&Request{Method: "PUT", Path: "/x", Headers: Headers{{"content-length", "11"}}, Body: []byte("01234567890")}, 1, 0)
	_, err := a.Accept(frames[0])
	var se *StreamError
	if !errors.As(err, &se) || se.Status != 413 {
		t.Fatalf("want 413 on the head, got %v", err)
	}
}

func TestOpenStreamCap(t *testing.T) {
	a := NewAssembler(RoleServer)
	a.MaxOpenStreams = 2
	var last error
	for i := range 3 {
		frames, _ := EncodeRequest(&Request{Method: "POST", Path: "/", Body: []byte("x")}, uint16(1+2*i), 0)
		_, last = a.Accept(frames[0])
	}
	var se *StreamError
	if !errors.As(last, &se) || se.StreamID != 5 {
		t.Fatalf("third concurrent stream: %v", last)
	}
}

func TestEncoderRefusesWhatTheDecoderWould(t *testing.T) {
	bad := []*Request{
		{Method: "BREW", Path: "/"},
		{Method: "GET", Path: "relative"},
		{Method: "GET", Path: "/../etc"},
		{Method: "GET", Path: "/" + strings.Repeat("a", MaxPathLen)},
		{Method: "GET", Path: "/", Headers: Headers{{"X-Upper", "v"}}},
		{Method: "GET", Path: "/", Headers: Headers{{"x", "line\nbreak"}}},
		{Method: "GET", Path: "/", Headers: make(Headers, 256)},
	}
	for _, req := range bad {
		if _, err := EncodeRequest(req, 1, 0); err == nil {
			t.Errorf("encoded %s %.20q", req.Method, req.Path)
		}
	}
}

func TestEncodeErrorTruncatesHugeReasons(t *testing.T) {
	f := EncodeError(500, strings.Repeat("é", MaxFrameSize))
	if len(f.Payload) > MaxFrameSize {
		t.Fatalf("payload %d", len(f.Payload))
	}
	m, err := decodeError(f.Payload, 0)
	if err != nil || !strings.HasPrefix(m.Reason, "é") {
		t.Fatalf("%v %v", m, err)
	}
}

func TestDecodePath(t *testing.T) {
	ok := map[string]string{
		"/":              "/",
		"/a%20b.txt":     "/a b.txt",
		"/%252e%252e/x":  "/%2e%2e/x",
		"/a/b?q=../../x": "/a/b",
		"/docs/":         "/docs/",
	}
	for in, want := range ok {
		if got, err := DecodePath(in); err != nil || got != want {
			t.Errorf("DecodePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"/..", "/./a", "/a/%2e%2e/b", "/a%2f..%2fb", "/%00", "/%0a", "/%5c", "/%", "/%4", "/%zz"} {
		if _, err := DecodePath(in); err == nil {
			t.Errorf("DecodePath(%q) accepted", in)
		}
	}
}
