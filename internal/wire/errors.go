package wire

import "fmt"

type StreamError struct {
	Status   int
	Reason   string
	StreamID uint16
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("stream %d: %d %s", e.StreamID, e.Status, e.Reason)
}

type ConnError struct {
	Status int
	Reason string
}

func (e *ConnError) Error() string {
	return fmt.Sprintf("connection: %d %s", e.Status, e.Reason)
}

func connErr(format string, args ...any) *ConnError {
	return &ConnError{Status: 400, Reason: fmt.Sprintf(format, args...)}
}

func streamErr(id uint16, status int, format string, args ...any) *StreamError {
	return &StreamError{Status: status, Reason: fmt.Sprintf(format, args...), StreamID: id}
}
