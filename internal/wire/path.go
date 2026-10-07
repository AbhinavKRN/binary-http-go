package wire

import (
	"fmt"
	"strings"
)

func ValidatePath(path string) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}
	if len(path) > MaxPathLen {
		return fmt.Errorf("path of %d bytes exceeds %d", len(path), MaxPathLen)
	}
	if path[0] != '/' {
		return fmt.Errorf("path does not start with '/'")
	}
	for i := 0; i < len(path); i++ {
		if c := path[i]; c < 0x21 || c > 0x7e {
			return fmt.Errorf("path contains byte 0x%02x", c)
		}
	}
	_, err := DecodePath(path)
	return err
}

func DecodePath(target string) (string, error) {
	path, _, _ := strings.Cut(target, "?")
	decoded, err := percentDecode(path)
	if err != nil {
		return "", err
	}
	for i := 0; i < len(decoded); i++ {
		switch c := decoded[i]; {
		case c < 0x20 || c == 0x7f:
			return "", fmt.Errorf("path decodes to control byte 0x%02x", c)
		case c == '\\':
			return "", fmt.Errorf("path contains a backslash")
		}
	}
	for segment := range strings.SplitSeq(decoded, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("path contains a %q segment", segment)
		}
	}
	return decoded, nil
}

func percentDecode(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			return "", fmt.Errorf("malformed percent-escape in path")
		}
		b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
		i += 2
	}
	return b.String(), nil
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}
