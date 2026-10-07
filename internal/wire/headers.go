package wire

import (
	"encoding/binary"
	"fmt"
	"strings"
)

var StaticTable = [...]string{
	"",
	"content-length",
	"content-type",
	"host",
	"date",
	"server",
	"connection",
	"user-agent",
	"accept",
	"last-modified",
	"cache-control",
}

var staticIndex = func() map[string]byte {
	m := make(map[string]byte, len(StaticTable))
	for i, name := range StaticTable[1:] {
		m[name] = byte(i + 1)
	}
	return m
}()

func StaticIndex(name string) byte { return staticIndex[name] }

const (
	indexedBit    = 0x80
	literalPrefix = 0x00
	maxHeaders    = 255
	maxNameLen    = 255
	maxValueLen   = 65535
)

type Header struct {
	Name  string
	Value string
}

type Headers []Header

func (h Headers) Get(name string) (string, bool) {
	for _, e := range h {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func (h Headers) Values(name string) []string {
	var out []string
	for _, e := range h {
		if e.Name == name {
			out = append(out, e.Value)
		}
	}
	return out
}

func appendHeaderBlock(dst []byte, headers Headers) ([]byte, error) {
	if len(headers) > maxHeaders {
		return nil, fmt.Errorf("%d headers, at most %d fit in HeaderCount", len(headers), maxHeaders)
	}
	dst = append(dst, byte(len(headers)))
	for _, h := range headers {
		if err := validName(h.Name); err != nil {
			return nil, err
		}
		if err := validValue(h.Value); err != nil {
			return nil, fmt.Errorf("header %q: %w", h.Name, err)
		}
		if len(h.Value) > maxValueLen {
			return nil, fmt.Errorf("header %q: value of %d bytes exceeds %d", h.Name, len(h.Value), maxValueLen)
		}
		if idx := StaticIndex(h.Name); idx != 0 {
			dst = append(dst, indexedBit|idx)
		} else {
			dst = append(dst, literalPrefix, byte(len(h.Name)))
			dst = append(dst, h.Name...)
		}
		dst = binary.BigEndian.AppendUint16(dst, uint16(len(h.Value)))
		dst = append(dst, h.Value...)
	}
	return dst, nil
}

func readHeaderBlock(c *cursor) (Headers, error) {
	count, err := c.u8()
	if err != nil {
		return nil, err
	}
	headers := make(Headers, 0, count)
	for range count {
		prefix, err := c.u8()
		if err != nil {
			return nil, err
		}
		var name string
		switch {
		case prefix&indexedBit != 0:
			idx := int(prefix &^ indexedBit)
			if idx == 0 || idx >= len(StaticTable) {
				return nil, fmt.Errorf("static table index %d does not exist", idx)
			}
			name = StaticTable[idx]
		case prefix == literalPrefix:
			n, err := c.u8()
			if err != nil {
				return nil, err
			}
			raw, err := c.take(int(n))
			if err != nil {
				return nil, err
			}
			name = string(raw)
			if err := validName(name); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("header prefix 0x%02x is unassigned", prefix)
		}
		n, err := c.u16()
		if err != nil {
			return nil, err
		}
		raw, err := c.take(int(n))
		if err != nil {
			return nil, err
		}
		value := string(raw)
		if err := validValue(value); err != nil {
			return nil, fmt.Errorf("header %q: %w", name, err)
		}
		headers = append(headers, Header{Name: name, Value: value})
	}
	return headers, nil
}

func validName(name string) error {
	if name == "" {
		return fmt.Errorf("empty header name")
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("header name of %d bytes exceeds %d", len(name), maxNameLen)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			return fmt.Errorf("header name %q is not lowercase", name)
		}
		if !isTokenChar(c) {
			return fmt.Errorf("header name %q is not a token", name)
		}
	}
	return nil
}

func isTokenChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

func validValue(v string) error {
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c == 0x7f {
			return fmt.Errorf("value contains control byte 0x%02x", c)
		}
	}
	return nil
}

type cursor struct {
	b   []byte
	pos int
}

var errTruncated = fmt.Errorf("payload ends mid-field")

func (c *cursor) take(n int) ([]byte, error) {
	if n > len(c.b)-c.pos {
		return nil, errTruncated
	}
	out := c.b[c.pos : c.pos+n]
	c.pos += n
	return out, nil
}

func (c *cursor) u8() (byte, error) {
	b, err := c.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (c *cursor) u16() (uint16, error) {
	b, err := c.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

func (c *cursor) remaining() int { return len(c.b) - c.pos }
