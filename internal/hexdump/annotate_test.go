package hexdump

import (
	"encoding/hex"
	"strings"
	"testing"

	"bht1/internal/wire"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const workedExample = `00 00 2c 01 01 00 00 01  01  00 0b  2f 69 6e 64 65 78 2e 68 74 6d 6c  02
	83 00 0e 6c 6f 63 61 6c 68 6f 73 74 3a 39 30 30 30  87 00 09 62 63 75 72 6c 2f 31 2e 30`

func TestWorkedExampleNotes(t *testing.T) {
	raw := append([]byte("BHT1"), unhex(t, workedExample)...)
	var notes []string
	for _, a := range Annotate(raw, 0) {
		notes = append(notes, a.Note)
	}
	want := []string{
		`Connection preface "BHT1"`,
		"Length = 44",
		"Type = 0x01 REQUEST",
		"Flags = 0x01 END_MESSAGE",
		"Reserved, MUST be ignored on receipt",
		"Stream ID = 1",
		"Method = 0x01 GET",
		"Path length = 11",
		`Path = "/index.html"`,
		"Header count = 2",
		`Static index 3 -> "host"`,
		"Value length = 14",
		`Value = "localhost:9000"`,
		`Static index 7 -> "user-agent"`,
		"Value length = 9",
		`Value = "bcurl/1.0"`,
	}
	if strings.Join(notes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notes:\n%s\nwant:\n%s", strings.Join(notes, "\n"), strings.Join(want, "\n"))
	}
}

func TestEveryByteIsExplainedExactlyOnce(t *testing.T) {
	inputs := map[string][]byte{
		"worked example": append([]byte("BHT1"), unhex(t, workedExample)...),
		"unknown type":   unhex(t, "00 00 04 7f 00 00 00 00 de ad be ef"),
		"truncated":      unhex(t, "00 00 2c 01 01 00 00 01 01 00"),
		"bad prefix":     unhex(t, "00 00 08 01 01 00 00 01 01 00 01 2f 01 05 00 01"),
		"error frame":    unhex(t, "00 00 0f 04 01 00 00 00 01 90 00 0b 62 61 64 20 70 72 65 66 61 63 65"),
		"trailing":       unhex(t, "00 00 03 02 01 00 00 01 00 c8 00 00 00 03"),
	}
	resp, _ := wire.EncodeResponse(&wire.Response{Status: 200, Headers: wire.Headers{{Name: "x-literal", Value: "v"}}, Body: []byte("hello")}, 1, 2)
	var raw []byte
	for _, f := range resp {
		raw = f.AppendTo(raw)
	}
	inputs["response with body"] = raw

	for name, raw := range inputs {
		next := 0
		for _, a := range Annotate(raw, 0) {
			if a.Offset != next {
				t.Errorf("%s: annotation at 0x%04x, expected 0x%04x (%s)", name, a.Offset, next, a.Note)
			}
			next = a.Offset + len(a.Data)
		}
		if next != len(raw) {
			t.Errorf("%s: explained %d of %d bytes", name, next, len(raw))
		}
	}
}

func TestRenderTruncatesLongRuns(t *testing.T) {
	line := Render([]Annotation{{Offset: 0x10, Data: make([]byte, 20), Note: "Body bytes (20)"}}, 16)
	if !strings.HasPrefix(line, "0010  00 00") || !strings.Contains(line, "... (+4)") || !strings.HasSuffix(line, "Body bytes (20)") {
		t.Fatalf("%q", line)
	}
}
