package hexdump

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	"bht1/internal/wire"
)

type Annotation struct {
	Offset int
	Data   []byte
	Note   string
}

func Annotate(raw []byte, base int) []Annotation {
	var out []Annotation
	pos := 0
	if bytes.HasPrefix(raw, wire.Preface) {
		out = append(out, Annotation{base, raw[:4], `Connection preface "BHT1"`})
		pos = 4
	}
	for pos+wire.HeaderSize <= len(raw) {
		notes, consumed := annotateFrame(raw[pos:], base+pos)
		out = append(out, notes...)
		if consumed == 0 {
			return out
		}
		pos += consumed
	}
	if pos < len(raw) {
		out = append(out, Annotation{base + pos, raw[pos:], "incomplete trailing bytes"})
	}
	return out
}

func annotateFrame(raw []byte, base int) ([]Annotation, int) {
	length, f := wire.ParseHeader(raw)
	typeName := f.Type.String()
	if !f.Type.Known() {
		typeName = "UNKNOWN (must be skipped)"
	}
	flags := "none"
	if f.EndMessage() {
		flags = "END_MESSAGE"
	}
	if extra := f.Flags &^ wire.FlagEndMessage; extra != 0 {
		flags += fmt.Sprintf(", unassigned bits 0x%02x (ignored)", extra)
	}
	out := []Annotation{
		{base, raw[0:3], fmt.Sprintf("Length = %d", length)},
		{base + 3, raw[3:4], fmt.Sprintf("Type = 0x%02x %s", byte(f.Type), typeName)},
		{base + 4, raw[4:5], fmt.Sprintf("Flags = 0x%02x %s", f.Flags, flags)},
		{base + 5, raw[5:6], "Reserved, MUST be ignored on receipt"},
		{base + 6, raw[6:8], fmt.Sprintf("Stream ID = %d", f.StreamID)},
	}
	end := wire.HeaderSize + length
	if length > wire.MaxFrameSize {
		return append(out, Annotation{base + wire.HeaderSize, nil, "Length exceeds MAX_FRAME_SIZE: connection error"}), 0
	}
	if end > len(raw) {
		return append(out, Annotation{base + wire.HeaderSize, raw[wire.HeaderSize:], "truncated payload"}), 0
	}
	p := &payload{b: raw[wire.HeaderSize:end], base: base + wire.HeaderSize}
	switch f.Type {
	case wire.TypeRequest:
		p.request()
	case wire.TypeResponse:
		p.response()
	case wire.TypeError:
		p.errorFrame()
	case wire.TypeData:
		p.note(len(p.b), fmt.Sprintf("Body bytes (%d)", len(p.b)))
	default:
		p.note(len(p.b), "opaque payload of an unknown frame type")
	}
	p.finish()
	return append(out, p.out...), end
}

type payload struct {
	b    []byte
	pos  int
	base int
	out  []Annotation
	bad  bool
}

func (p *payload) note(n int, text string) []byte {
	if p.bad {
		return nil
	}
	if n > len(p.b)-p.pos {
		p.out = append(p.out, Annotation{p.base + p.pos, p.b[p.pos:], "payload ends mid-field: " + text})
		p.bad, p.pos = true, len(p.b)
		return nil
	}
	data := p.b[p.pos : p.pos+n]
	p.out = append(p.out, Annotation{p.base + p.pos, data, text})
	p.pos += n
	return data
}

func (p *payload) peek(n int) []byte {
	if p.bad || n > len(p.b)-p.pos {
		return nil
	}
	return p.b[p.pos : p.pos+n]
}

func (p *payload) u8(label string, describe func(byte) string) (byte, bool) {
	b := p.peek(1)
	if b == nil {
		p.note(1, label)
		return 0, false
	}
	p.note(1, describe(b[0]))
	return b[0], true
}

func (p *payload) u16(describe func(uint16) string) (uint16, bool) {
	b := p.peek(2)
	if b == nil {
		p.note(2, describe(0))
		return 0, false
	}
	v := binary.BigEndian.Uint16(b)
	p.note(2, describe(v))
	return v, true
}

func (p *payload) request() {
	p.u8("Method", func(c byte) string {
		name := wire.MethodName(c)
		if name == "" {
			name = "? (unassigned)"
		}
		return fmt.Sprintf("Method = 0x%02x %s", c, name)
	})
	n, ok := p.u16(func(v uint16) string { return fmt.Sprintf("Path length = %d", v) })
	if !ok {
		return
	}
	if path := p.peek(int(n)); path != nil {
		p.note(int(n), fmt.Sprintf("Path = %q", path))
	} else {
		p.note(int(n), "Path")
		return
	}
	p.headerBlock()
}

func (p *payload) response() {
	if _, ok := p.u16(func(v uint16) string { return fmt.Sprintf("Status = %d", v) }); ok {
		p.headerBlock()
	}
}

func (p *payload) errorFrame() {
	if _, ok := p.u16(func(v uint16) string { return fmt.Sprintf("Status = %d", v) }); !ok {
		return
	}
	n, ok := p.u16(func(v uint16) string { return fmt.Sprintf("Reason length = %d", v) })
	if !ok {
		return
	}
	if reason := p.peek(int(n)); reason != nil {
		p.note(int(n), fmt.Sprintf("Reason = %q", strings.ToValidUTF8(string(reason), "\uFFFD")))
	} else {
		p.note(int(n), "Reason")
	}
}

func (p *payload) headerBlock() {
	count, ok := p.u8("Header count", func(c byte) string { return fmt.Sprintf("Header count = %d", c) })
	if !ok {
		return
	}
	for range count {
		prefix := p.peek(1)
		if prefix == nil {
			p.note(1, "header entry")
			return
		}
		switch b := prefix[0]; {
		case b&0x80 != 0:
			idx := int(b & 0x7f)
			name := "? (no such index)"
			if idx > 0 && idx < len(wire.StaticTable) {
				name = wire.StaticTable[idx]
			}
			p.note(1, fmt.Sprintf("Static index %d -> %q", idx, name))
		case b == 0x00:
			head := p.peek(2)
			if head == nil {
				p.note(2, "literal name")
				return
			}
			n := int(head[1])
			if whole := p.peek(2 + n); whole != nil {
				p.note(2+n, fmt.Sprintf("Literal name (%d bytes) = %q", n, whole[2:]))
			} else {
				p.note(2+n, "literal name")
				return
			}
		default:
			p.note(1, fmt.Sprintf("Header prefix 0x%02x is unassigned: stream error", b))
			p.bad = true
			return
		}
		n, ok := p.u16(func(v uint16) string { return fmt.Sprintf("Value length = %d", v) })
		if !ok {
			return
		}
		if value := p.peek(int(n)); value != nil {
			p.note(int(n), fmt.Sprintf("Value = %q", value))
		} else {
			p.note(int(n), "Value")
			return
		}
	}
}

func (p *payload) finish() {
	if p.pos >= len(p.b) {
		return
	}
	note := "trailing bytes after the last field: stream error"
	if p.bad {
		note = "not parsed past the error above"
	}
	p.out = append(p.out, Annotation{p.base + p.pos, p.b[p.pos:], note})
}

func Render(notes []Annotation, maxBytes int) string {
	var b strings.Builder
	for i, a := range notes {
		if i > 0 {
			b.WriteByte('\n')
		}
		shown := a.Data[:min(len(a.Data), maxBytes)]
		hexText := fmt.Sprintf("% x", shown)
		if len(a.Data) > maxBytes {
			hexText += fmt.Sprintf(" ... (+%d)", len(a.Data)-maxBytes)
		}
		fmt.Fprintf(&b, "%04x  %-52s  %s", a.Offset, hexText, a.Note)
	}
	return b.String()
}

func Classic(data []byte, base int) string {
	var b strings.Builder
	for off := 0; off < len(data); off += 16 {
		chunk := data[off:min(off+16, len(data))]
		text := make([]byte, len(chunk))
		for i, c := range chunk {
			text[i] = '.'
			if c >= 0x20 && c < 0x7f {
				text[i] = c
			}
		}
		if off > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%04x  %-48s |%s|", base+off, fmt.Sprintf("% x", chunk), text)
	}
	return b.String()
}
