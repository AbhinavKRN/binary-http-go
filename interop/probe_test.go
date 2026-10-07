package interop

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"bht1/internal/wire"
)

func rawStatus(t *testing.T, addr, path string) int {
	t.Helper()
	nc, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	payload := []byte{0x01}
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(path)))
	payload = append(append(payload, path...), 0)
	raw := wire.Frame{Type: wire.TypeRequest, Flags: wire.FlagEndMessage, StreamID: 1, Payload: payload}.AppendTo(append([]byte{}, wire.Preface...))
	nc.Write(raw)
	r := wire.NewReader(false)
	buf := make([]byte, 4096)
	nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		f, ok, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if f.Type == wire.TypeResponse || f.Type == wire.TypeError {
				return int(binary.BigEndian.Uint16(f.Payload))
			}
			continue
		}
		n, err := nc.Read(buf)
		r.Feed(buf[:n])
		if err != nil && n == 0 {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

func exchange(t *testing.T, addr string, raw []byte) []int {
	t.Helper()
	nc, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	nc.Write(append(append([]byte{}, wire.Preface...), raw...))
	var got []int
	r := wire.NewReader(false)
	buf := make([]byte, 4096)
	for {
		f, ok, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			switch f.Type {
			case wire.TypeResponse:
				got = append(got, int(binary.BigEndian.Uint16(f.Payload)))
			case wire.TypeError:
				got = append(got, -int(binary.BigEndian.Uint16(f.Payload)))
			}
			continue
		}
		nc.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
		n, err := nc.Read(buf)
		r.Feed(buf[:n])
		if err != nil && n == 0 {
			return got
		}
	}
}

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStreamStateAfterAStreamError(t *testing.T) {
	need(t)
	pyAddr, _ := pythonServer(t)
	goAddr := goServer(t).Addr()
	good := func(id byte) string {
		return "00 00 0e 01 01 00 00 " + hex.EncodeToString([]byte{id}) + " 01 00 0a 2f 68 65 6c 6c 6f 2e 74 78 74 00"
	}
	cases := map[string]struct {
		raw  string
		want []int
	}{

		"malformed head uses up its ID": {"00 00 05 01 01 00 00 01 00 00 01 2f 00 " + good(1), []int{400, -400}},

		"DATA after a rejected head is drained": {"00 00 05 01 00 00 00 01 00 00 01 2f 00  00 00 02 03 01 00 00 01 68 69 " + good(3), []int{400, 200}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			raw := hexBytes(t, c.raw)
			for impl, addr := range map[string]string{"python": pyAddr, "go": goAddr} {
				if got := exchange(t, addr, raw); fmt.Sprint(got) != fmt.Sprint(c.want) {
					t.Errorf("%s server answered %v, want %v (negative = ERROR on stream 0)", impl, got, c.want)
				}
			}
		})
	}
}

func TestWhereTheSpecIsSilent(t *testing.T) {
	need(t)
	pyAddr, _ := pythonServer(t)
	goAddr := goServer(t).Addr()
	cases := []struct {
		path     string
		agree    bool
		py, goes int
	}{

		{"/%zz", false, 404, 400},
		{"/hello.txt%", false, 404, 400},

		{"/hello.txt?x=/../y", true, 200, 200},
		{"/hello.txt?%00", true, 200, 200},
	}
	for _, c := range cases {
		py, gs := rawStatus(t, pyAddr, c.path), rawStatus(t, goAddr, c.path)
		t.Logf("%-22q python %d, go %d", c.path, py, gs)
		if py != c.py || gs != c.goes {
			t.Errorf("%q: python %d, go %d; SPEC-NOTES.md records %d and %d", c.path, py, gs, c.py, c.goes)
		}
		if c.agree != (py == gs) {
			t.Errorf("%q: agreement changed; update SPEC-NOTES.md", c.path)
		}
	}
}
