package wire

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

var methodCodes = map[string]byte{
	"GET": 0x01, "HEAD": 0x02, "POST": 0x03, "PUT": 0x04,
	"DELETE": 0x05, "OPTIONS": 0x06, "PATCH": 0x07,
}

var methodNames = func() map[byte]string {
	m := make(map[byte]string, len(methodCodes))
	for name, code := range methodCodes {
		m[code] = name
	}
	return m
}()

func MethodName(code byte) string { return methodNames[code] }

func MethodCode(name string) byte { return methodCodes[name] }

const (
	MaxPathLen = 8192

	DefaultMaxBody = 8 << 20

	DefaultMaxOpenStreams = 128
)

type Message interface{ Stream() uint16 }

type Request struct {
	StreamID uint16
	Method   string
	Path     string
	Headers  Headers
	Body     []byte
}

type Response struct {
	StreamID uint16
	Status   int
	Headers  Headers
	Body     []byte
}

type ErrorMsg struct {
	StreamID uint16
	Status   int
	Reason   string
}

func (m *Request) Stream() uint16  { return m.StreamID }
func (m *Response) Stream() uint16 { return m.StreamID }
func (m *ErrorMsg) Stream() uint16 { return m.StreamID }

func EncodeRequest(req *Request, streamID uint16, maxData int) ([]Frame, error) {
	code := MethodCode(req.Method)
	if code == 0 {
		return nil, fmt.Errorf("method %q has no BHT/1 code", req.Method)
	}
	if err := ValidatePath(req.Path); err != nil {
		return nil, err
	}
	payload := []byte{code}
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(req.Path)))
	payload = append(payload, req.Path...)
	payload, err := appendHeaderBlock(payload, req.Headers)
	if err != nil {
		return nil, err
	}
	return messageFrames(TypeRequest, streamID, payload, req.Body, maxData)
}

func EncodeResponse(resp *Response, streamID uint16, maxData int) ([]Frame, error) {
	if resp.Status < 0 || resp.Status > 0xffff {
		return nil, fmt.Errorf("status %d does not fit in u16", resp.Status)
	}
	payload := binary.BigEndian.AppendUint16(nil, uint16(resp.Status))
	payload, err := appendHeaderBlock(payload, resp.Headers)
	if err != nil {
		return nil, err
	}
	return messageFrames(TypeResponse, streamID, payload, resp.Body, maxData)
}

func EncodeError(status int, reason string) Frame {
	limit := MaxFrameSize - 4
	if len(reason) > limit {
		reason = strings.ToValidUTF8(reason[:limit], "")
	}
	payload := binary.BigEndian.AppendUint16(nil, uint16(status))
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(reason)))
	payload = append(payload, reason...)
	return Frame{Type: TypeError, Flags: FlagEndMessage, Payload: payload}
}

func messageFrames(t FrameType, streamID uint16, head, body []byte, maxData int) ([]Frame, error) {
	if len(head) > MaxFrameSize {
		return nil, fmt.Errorf("%s head of %d bytes does not fit in one frame", t, len(head))
	}
	if maxData <= 0 || maxData > MaxFrameSize {
		maxData = MaxFrameSize
	}
	if len(body) == 0 {
		return []Frame{{Type: t, Flags: FlagEndMessage, StreamID: streamID, Payload: head}}, nil
	}
	frames := make([]Frame, 0, 1+(len(body)+maxData-1)/maxData)
	frames = append(frames, Frame{Type: t, StreamID: streamID, Payload: head})
	for start := 0; start < len(body); start += maxData {
		end := min(start+maxData, len(body))
		f := Frame{Type: TypeData, StreamID: streamID, Payload: body[start:end]}
		if end == len(body) {
			f.Flags = FlagEndMessage
		}
		frames = append(frames, f)
	}
	return frames, nil
}

func decodeRequestHead(payload []byte, id uint16) (*Request, error) {
	c := &cursor{b: payload}
	code, err := c.u8()
	if err != nil {
		return nil, err
	}
	method := MethodName(code)
	if method == "" {
		return nil, fmt.Errorf("method code 0x%02x is unassigned", code)
	}
	n, err := c.u16()
	if err != nil {
		return nil, err
	}
	raw, err := c.take(int(n))
	if err != nil {
		return nil, err
	}
	path := string(raw)
	if err := ValidatePath(path); err != nil {
		return nil, err
	}
	headers, err := readHeaderBlock(c)
	if err != nil {
		return nil, err
	}
	if c.remaining() != 0 {
		return nil, fmt.Errorf("%d bytes left over after the header block", c.remaining())
	}
	return &Request{StreamID: id, Method: method, Path: path, Headers: headers}, nil
}

func decodeResponseHead(payload []byte, id uint16) (*Response, error) {
	c := &cursor{b: payload}
	status, err := c.u16()
	if err != nil {
		return nil, err
	}
	headers, err := readHeaderBlock(c)
	if err != nil {
		return nil, err
	}
	if c.remaining() != 0 {
		return nil, fmt.Errorf("%d bytes left over after the header block", c.remaining())
	}
	return &Response{StreamID: id, Status: int(status), Headers: headers}, nil
}

func decodeError(payload []byte, id uint16) (*ErrorMsg, error) {
	c := &cursor{b: payload}
	status, err := c.u16()
	if err != nil {
		return nil, err
	}
	n, err := c.u16()
	if err != nil {
		return nil, err
	}
	reason, err := c.take(int(n))
	if err != nil {
		return nil, err
	}
	if c.remaining() != 0 {
		return nil, fmt.Errorf("%d bytes left over after the reason", c.remaining())
	}
	return &ErrorMsg{StreamID: id, Status: int(status), Reason: string(reason)}, nil
}

func ContentLength(h Headers) (n int64, present bool, err error) {
	values := h.Values("content-length")
	if len(values) == 0 {
		return 0, false, nil
	}
	for i, v := range values {
		if v == "" || len(v) > 18 || strings.Trim(v, "0123456789") != "" {
			return 0, true, fmt.Errorf("content-length %q is not a decimal integer", v)
		}
		parsed, _ := strconv.ParseInt(v, 10, 64)
		if i > 0 && parsed != n {
			return 0, true, fmt.Errorf("conflicting content-length headers %v", values)
		}
		n = parsed
	}
	return n, true, nil
}
