package wire

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func seedCorpus(f *testing.F) {
	for _, file := range []string{"requests.jsonl", "responses.jsonl", "invalid.jsonl"} {
		for _, v := range loadVectors(f, file) {
			f.Add(unhexVector(f, v.Hex), uint8(1))
		}
	}
}

type outcome struct {
	Messages []string
	Failures []string
	Fatal    string
	Skipped  int
}

func run(data []byte, chunk int) outcome {
	r := NewReader(false)
	a := NewAssembler(RoleServer)
	var out outcome
	for start := 0; start < len(data); start += chunk {
		r.Feed(data[start:min(start+chunk, len(data))])
		for {
			f, ok, err := r.Next()
			if err != nil {
				out.Fatal = err.Error()
				out.Skipped = a.SkippedUnknown
				return out
			}
			if !ok {
				break
			}
			m, err := a.Accept(f)
			var se *StreamError
			switch {
			case errors.As(err, &se):
				out.Failures = append(out.Failures, se.Error())
			case err != nil:
				out.Fatal = err.Error()
				out.Skipped = a.SkippedUnknown
				return out
			case m != nil:
				out.Messages = append(out.Messages, fmt.Sprintf("%#v", m))
			}
		}
	}
	out.Skipped = a.SkippedUnknown
	return out
}

func FuzzChunkingIsInvisible(f *testing.F) {
	seedCorpus(f)
	f.Fuzz(func(t *testing.T, data []byte, chunk uint8) {
		if len(data) == 0 {
			return
		}
		whole := run(data, len(data))
		split := run(data, int(chunk%17)+1)
		if !reflect.DeepEqual(whole, split) {
			t.Fatalf("chunking changed the outcome\nwhole: %+v\nsplit: %+v", whole, split)
		}
	})
}

func FuzzRequestRoundTrip(f *testing.F) {
	seedCorpus(f)
	f.Fuzz(func(t *testing.T, data []byte, _ uint8) {
		r := NewReader(false)
		r.Feed(data)
		a := NewAssembler(RoleServer)
		for {
			fr, ok, err := r.Next()
			if err != nil || !ok {
				return
			}
			m, err := a.Accept(fr)
			if err != nil {
				return
			}
			req, isReq := m.(*Request)
			if !isReq {
				continue
			}
			frames, err := EncodeRequest(req, 1, 0)
			if err != nil {
				t.Fatalf("decoded request does not re-encode: %v\n%#v", err, req)
			}
			var raw []byte
			for _, fr := range frames {
				raw = fr.AppendTo(raw)
			}
			again, err := decodeAll(raw)
			if err != nil {
				t.Fatalf("re-encoded request does not decode: %v", err)
			}
			req.StreamID = 1
			if len(req.Body) == 0 {
				req.Body = nil
			}
			if !reflect.DeepEqual(again, req) {
				t.Fatalf("round trip changed the request\nwas: %#v\nnow: %#v", req, again)
			}
		}
	})
}

func decodeAll(raw []byte) (*Request, error) {
	r := NewReader(false)
	r.Feed(raw)
	a := NewAssembler(RoleServer)
	for {
		fr, ok, err := r.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("incomplete")
		}
		m, err := a.Accept(fr)
		if err != nil {
			return nil, err
		}
		if m != nil {
			return m.(*Request), nil
		}
	}
}
