package wire

import (
	"bytes"
	"encoding/binary"
)

var Preface = []byte("BHT1")

const (
	HeaderSize   = 8
	MaxFrameSize = 65535
	MaxStreamID  = 65535
)

type FrameType uint8

const (
	TypeRequest  FrameType = 0x01
	TypeResponse FrameType = 0x02
	TypeData     FrameType = 0x03
	TypeError    FrameType = 0x04
)

func (t FrameType) Known() bool { return t >= TypeRequest && t <= TypeError }

func (t FrameType) String() string {
	switch t {
	case TypeRequest:
		return "REQUEST"
	case TypeResponse:
		return "RESPONSE"
	case TypeData:
		return "DATA"
	case TypeError:
		return "ERROR"
	}
	return "UNKNOWN"
}

const FlagEndMessage uint8 = 0x01

type Frame struct {
	Type     FrameType
	Flags    uint8
	Reserved uint8
	StreamID uint16
	Payload  []byte
}

func (f Frame) EndMessage() bool { return f.Flags&FlagEndMessage != 0 }

func (f Frame) AppendTo(dst []byte) []byte {
	n := len(f.Payload)
	if n > MaxFrameSize {
		panic("wire: frame payload exceeds MaxFrameSize")
	}
	dst = append(dst, byte(n>>16), byte(n>>8), byte(n), byte(f.Type), f.Flags, 0)
	dst = binary.BigEndian.AppendUint16(dst, f.StreamID)
	return append(dst, f.Payload...)
}

func (f Frame) AppendAsReceived(dst []byte) []byte {
	start := len(dst)
	dst = f.AppendTo(dst)
	dst[start+5] = f.Reserved
	return dst
}

func (f Frame) Encode() []byte {
	return f.AppendTo(make([]byte, 0, HeaderSize+len(f.Payload)))
}

func ParseHeader(b []byte) (length int, f Frame) {
	length = int(b[0])<<16 | int(b[1])<<8 | int(b[2])
	f = Frame{
		Type:     FrameType(b[3]),
		Flags:    b[4],
		Reserved: b[5],
		StreamID: binary.BigEndian.Uint16(b[6:8]),
	}
	return length, f
}

type Reader struct {
	buf          []byte
	off          int
	needPreface  bool
	prefaceBytes int
	failed       error
}

func NewReader(expectPreface bool) *Reader {
	return &Reader{needPreface: expectPreface}
}

func (r *Reader) Feed(p []byte) {
	if r.off > 0 {

		n := copy(r.buf, r.buf[r.off:])
		r.buf, r.off = r.buf[:n], 0
	}
	r.buf = append(r.buf, p...)
}

func (r *Reader) Buffered() int { return len(r.buf) - r.off }

func (r *Reader) Next() (f Frame, ok bool, err error) {
	if r.failed != nil {
		return Frame{}, false, r.failed
	}
	if r.needPreface {
		if err := r.consumePreface(); err != nil || r.needPreface {
			return Frame{}, false, err
		}
	}
	avail := r.buf[r.off:]
	if len(avail) < HeaderSize {
		return Frame{}, false, nil
	}
	length, f := ParseHeader(avail)
	if length > MaxFrameSize {

		return Frame{}, false, r.fail(connErr("frame length %d exceeds MAX_FRAME_SIZE %d", length, MaxFrameSize))
	}
	if len(avail) < HeaderSize+length {
		return Frame{}, false, nil
	}
	f.Payload = avail[HeaderSize : HeaderSize+length : HeaderSize+length]
	r.off += HeaderSize + length
	return f, true, nil
}

func (r *Reader) consumePreface() error {
	avail := r.buf[r.off:]
	want := Preface[r.prefaceBytes:]
	n := min(len(avail), len(want))
	if !bytes.Equal(avail[:n], want[:n]) {
		return r.fail(connErr("connection preface is not %q", Preface))
	}
	r.off += n
	r.prefaceBytes += n
	r.needPreface = r.prefaceBytes < len(Preface)
	return nil
}

func (r *Reader) fail(err *ConnError) error {
	r.failed = err
	return err
}
