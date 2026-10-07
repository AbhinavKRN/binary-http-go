package wire

type Role uint8

const (
	RoleServer Role = iota + 1
	RoleClient
)

type stream struct {
	head       Message
	headSeen   bool
	expectBody bool
	draining   bool
	body       []byte
}

type Assembler struct {
	role    Role
	streams map[uint16]*stream
	highest uint16

	MaxBody int

	MaxOpenStreams int

	SkippedUnknown int
}

func NewAssembler(role Role) *Assembler {
	return &Assembler{
		role:           role,
		streams:        make(map[uint16]*stream),
		MaxBody:        DefaultMaxBody,
		MaxOpenStreams: DefaultMaxOpenStreams,
	}
}

func (a *Assembler) OpenStream(id uint16, expectBody bool) {
	a.streams[id] = &stream{expectBody: expectBody}
}

func (a *Assembler) InFlight() int { return len(a.streams) }

func (a *Assembler) Accept(f Frame) (Message, error) {
	switch f.Type {
	case TypeRequest:
		if a.role != RoleServer {
			return nil, connErr("REQUEST frame received by a client")
		}
		return a.acceptRequest(f)
	case TypeResponse:
		if a.role != RoleClient {
			return nil, connErr("RESPONSE frame received by a server")
		}
		return a.acceptResponse(f)
	case TypeData:
		return a.acceptData(f)
	case TypeError:
		return a.acceptError(f)
	}

	a.SkippedUnknown++
	return nil, nil
}

func (a *Assembler) acceptRequest(f Frame) (Message, error) {
	id := f.StreamID
	switch {
	case id == 0:
		return nil, connErr("REQUEST on stream 0")
	case id%2 == 0:
		return nil, connErr("REQUEST on even stream %d", id)
	case id <= a.highest:
		return nil, connErr("stream %d is not above %d", id, a.highest)
	}

	a.highest = id

	req, err := decodeRequestHead(f.Payload, id)
	if err != nil {
		return nil, a.failHead(f, streamErr(id, 400, "%v", err))
	}
	return a.startMessage(f, req, true)
}

func (a *Assembler) acceptResponse(f Frame) (Message, error) {
	id := f.StreamID
	s, ok := a.streams[id]
	if !ok || s.headSeen {
		return nil, connErr("RESPONSE on stream %d, which is not awaiting one", id)
	}
	resp, err := decodeResponseHead(f.Payload, id)
	if err != nil {
		return nil, a.failHead(f, streamErr(id, 400, "%v", err))
	}
	return a.startMessage(f, resp, s.expectBody)
}

func (a *Assembler) startMessage(f Frame, head Message, expectBody bool) (Message, error) {
	id := f.StreamID
	if f.EndMessage() {
		delete(a.streams, id)
		return complete(head, nil, expectBody)
	}
	if a.openBodies() >= a.MaxOpenStreams {
		return nil, a.failHead(f, streamErr(id, 400, "more than %d streams open at once", a.MaxOpenStreams))
	}
	if n, ok, err := ContentLength(headersOf(head)); err != nil {
		return nil, a.failHead(f, streamErr(id, 400, "%v", err))
	} else if ok && n > int64(a.MaxBody) {
		return nil, a.failHead(f, streamErr(id, 413, "content-length %d exceeds %d", n, a.MaxBody))
	}
	a.streams[id] = &stream{head: head, headSeen: true, expectBody: expectBody}
	return nil, nil
}

func (a *Assembler) failHead(f Frame, err *StreamError) error {
	if f.EndMessage() {
		delete(a.streams, f.StreamID)
	} else {
		a.streams[f.StreamID] = &stream{headSeen: true, draining: true}
	}
	return err
}

func (a *Assembler) acceptData(f Frame) (Message, error) {
	id := f.StreamID
	s, ok := a.streams[id]
	if !ok || !s.headSeen {
		return nil, connErr("DATA on stream %d, which has no message in progress", id)
	}
	if f.EndMessage() {
		delete(a.streams, id)
	}
	if s.draining {
		return nil, nil
	}
	if !s.expectBody {
		s.draining, s.body = true, nil
		return nil, streamErr(id, 400, "DATA in the response to a HEAD request")
	}
	if len(s.body)+len(f.Payload) > a.MaxBody {
		s.draining, s.body = true, nil
		return nil, streamErr(id, 413, "body exceeds %d bytes", a.MaxBody)
	}
	s.body = append(s.body, f.Payload...)
	if !f.EndMessage() {
		return nil, nil
	}
	switch m := s.head.(type) {
	case *Request:
		m.Body = s.body
	case *Response:
		m.Body = s.body
	}
	return complete(s.head, s.body, s.expectBody)
}

func (a *Assembler) acceptError(f Frame) (Message, error) {
	msg, err := decodeError(f.Payload, f.StreamID)
	if err != nil {
		return nil, connErr("malformed ERROR frame: %v", err)
	}
	if f.StreamID == 0 {
		return msg, nil
	}
	if _, ok := a.streams[f.StreamID]; !ok {
		return nil, connErr("ERROR on stream %d, which is not open", f.StreamID)
	}
	delete(a.streams, f.StreamID)
	return msg, nil
}

func complete(m Message, body []byte, expectBody bool) (Message, error) {
	n, ok, err := ContentLength(headersOf(m))
	if err != nil {
		return nil, streamErr(m.Stream(), 400, "%v", err)
	}
	if ok && expectBody && n != int64(len(body)) {
		return nil, streamErr(m.Stream(), 400, "content-length %d but %d body bytes arrived", n, len(body))
	}
	return m, nil
}

func (a *Assembler) openBodies() int {
	n := 0
	for _, s := range a.streams {
		if s.headSeen && !s.draining {
			n++
		}
	}
	return n
}

func headersOf(m Message) Headers {
	switch m := m.(type) {
	case *Request:
		return m.Headers
	case *Response:
		return m.Headers
	}
	return nil
}
