package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"bht1/internal/wire"
)

const (
	ServerToken = "bserve-go/1.0"
	httpDate    = "Mon, 02 Jan 2006 15:04:05 GMT"
)

type Config struct {
	Root               string
	Addr               string
	IdleTimeout        time.Duration
	WriteTimeout       time.Duration
	MaxData            int
	MaxBody            int
	MaxRequestsPerConn int
	MaxConns           int
	Logger             *slog.Logger
}

func (c *Config) setDefaults() {
	if c.Addr == "" {
		c.Addr = "127.0.0.1:9000"
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 30 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.MaxData <= 0 || c.MaxData > wire.MaxFrameSize {
		c.MaxData = wire.MaxFrameSize
	}
	if c.MaxBody <= 0 {
		c.MaxBody = wire.DefaultMaxBody
	}
	if c.MaxRequestsPerConn <= 0 {
		c.MaxRequestsPerConn = 10_000
	}
	if c.MaxConns <= 0 {
		c.MaxConns = 1024
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
}

type Stats struct {
	Accepted, Active, Requests, Responses, SkippedUnknown, ConnErrors int64
}

type Server struct {
	cfg   Config
	files *Files
	log   *slog.Logger

	ln       net.Listener
	slots    chan struct{}
	closing  atomic.Bool
	wg       sync.WaitGroup
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	accepted atomic.Int64
	active   atomic.Int64
	requests atomic.Int64
	answered atomic.Int64
	skipped  atomic.Int64
	fatal    atomic.Int64
}

func New(cfg Config) (*Server, error) {
	cfg.setDefaults()
	files, err := NewFiles(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("document root: %w", err)
	}
	return &Server{
		cfg:   cfg,
		files: files,
		log:   cfg.Logger,
		slots: make(chan struct{}, cfg.MaxConns),
		conns: make(map[net.Conn]struct{}),
	}, nil
}

func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.log.Info("serving", "root", s.files.Root(), "addr", "bht://"+ln.Addr().String())
	return nil
}

func (s *Server) Addr() string { return s.ln.Addr().String() }

func (s *Server) Stats() Stats {
	return Stats{
		Accepted:       s.accepted.Load(),
		Active:         s.active.Load(),
		Requests:       s.requests.Load(),
		Responses:      s.answered.Load(),
		SkippedUnknown: s.skipped.Load(),
		ConnErrors:     s.fatal.Load(),
	}
}

func (s *Server) Serve() error {
	if s.ln == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	var backoff time.Duration
	for {
		s.slots <- struct{}{}
		c, err := s.ln.Accept()
		if err != nil {
			<-s.slots
			if s.closing.Load() {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
				s.log.Warn("accept failed; backing off", "err", err, "delay", backoff)
				time.Sleep(backoff)
				continue
			}
			return err
		}
		backoff = 0
		if !s.track(c) {
			c.Close()
			<-s.slots
			continue
		}
		s.wg.Add(1)
		go func() {
			defer func() {
				s.untrack(c)
				<-s.slots
				s.wg.Done()
			}()
			s.serveConn(c)
		}()
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	if !s.closing.CompareAndSwap(false, true) {
		return nil
	}
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Lock()
	for c := range s.conns {
		c.SetReadDeadline(time.Now())
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		<-done
	}
	st := s.Stats()
	s.log.Info("stopped", "accepted", st.Accepted, "requests", st.Requests,
		"responses", st.Responses, "skipped_unknown", st.SkippedUnknown, "conn_errors", st.ConnErrors)
	return ctx.Err()
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing.Load() {
		return false
	}
	s.conns[c] = struct{}{}
	s.accepted.Add(1)
	s.active.Add(1)
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.active.Add(-1)
}

type conn struct {
	srv    *Server
	nc     net.Conn
	peer   string
	w      *bufio.Writer
	r      *wire.Reader
	a      *wire.Assembler
	served int
	chunk  []byte
	frame  []byte
}

var errClose = errors.New("closing after this response")

func (s *Server) serveConn(nc net.Conn) {
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	c := &conn{
		srv:   s,
		nc:    nc,
		peer:  nc.RemoteAddr().String(),
		w:     bufio.NewWriterSize(nc, 64<<10),
		r:     wire.NewReader(true),
		a:     wire.NewAssembler(wire.RoleServer),
		chunk: make([]byte, s.cfg.MaxData),
	}
	c.a.MaxBody = s.cfg.MaxBody
	defer nc.Close()

	buf := make([]byte, 64<<10)
	for {
		if err := c.drain(); err != nil {
			if !errors.Is(err, errClose) {
				s.log.Debug("connection ended", "peer", c.peer, "err", err)
			}
			c.lingerClose()
			return
		}
		if err := c.flush(); err != nil {
			return
		}
		if s.closing.Load() {
			return
		}
		nc.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout))
		n, err := nc.Read(buf)
		if n > 0 {
			c.r.Feed(buf[:n])
		}
		if err != nil {
			if n > 0 {

				if derr := c.drain(); derr == nil {
					c.flush()
				}
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() && !s.closing.Load() {
				s.log.Debug("idle timeout", "peer", c.peer)
			}
			return
		}
	}
}

func (c *conn) drain() error {
	s := c.srv
	for {
		f, ok, err := c.r.Next()
		if err != nil {
			return c.connFailure(err)
		}
		if !ok {
			return nil
		}
		if !f.Type.Known() {
			s.skipped.Add(1)
			s.log.Debug("skipped unknown frame", "peer", c.peer, "type", fmt.Sprintf("0x%02x", byte(f.Type)), "len", len(f.Payload))
		}
		m, err := c.a.Accept(f)
		var se *wire.StreamError
		switch {
		case errors.As(err, &se):
			s.requests.Add(1)
			if err := c.send(errorResponse(se.Status, se.Reason, false), se.StreamID, "?", "", time.Now()); err != nil {
				return err
			}
			continue
		case err != nil:
			return c.connFailure(err)
		}
		switch m := m.(type) {
		case *wire.Request:
			s.requests.Add(1)
			if err := c.serve(m); err != nil {
				return err
			}
		case *wire.ErrorMsg:

			s.log.Info("peer sent ERROR", "peer", c.peer, "status", m.Status, "reason", m.Reason)
			return errClose
		}
	}
}

func (c *conn) connFailure(err error) error {
	var ce *wire.ConnError
	if !errors.As(err, &ce) {
		return err
	}
	c.srv.fatal.Add(1)
	c.srv.log.Warn("connection error", "peer", c.peer, "status", ce.Status, "reason", ce.Reason)
	c.frame = wire.EncodeError(ce.Status, ce.Reason).AppendTo(c.frame[:0])
	c.nc.SetWriteDeadline(time.Now().Add(c.srv.cfg.WriteTimeout))
	if _, werr := c.w.Write(c.frame); werr == nil {
		c.w.Flush()
	}
	return errClose
}

func (c *conn) serve(req *wire.Request) error {
	c.served++
	closeAfter := c.served >= c.srv.cfg.MaxRequestsPerConn || c.srv.closing.Load()
	t0 := time.Now()
	head := req.Method == "HEAD"

	if req.Method != "GET" && !head {
		resp := errorResponse(405, req.Method+" is not allowed on a static file", false)
		return c.finish(c.send(markClose(resp, closeAfter), req.StreamID, req.Method, req.Path, t0), closeAfter)
	}

	path, err := c.srv.files.Resolve(req.Path)
	if err != nil {
		status := 500
		switch {
		case errors.Is(err, ErrBadPath):
			status = 400
		case errors.Is(err, ErrNotFound):
			status = 404
		}
		resp := errorResponse(status, err.Error(), head)
		return c.finish(c.send(markClose(resp, closeAfter), req.StreamID, req.Method, req.Path, t0), closeAfter)
	}
	return c.finish(c.sendFile(path, req, closeAfter, t0), closeAfter)
}

func (c *conn) finish(err error, closeAfter bool) error {
	if err == nil && closeAfter {
		return errClose
	}
	return err
}

func (c *conn) sendFile(path string, req *wire.Request, closeAfter bool, t0 time.Time) error {
	f, err := os.Open(path)
	if err != nil {
		return c.send(markClose(errorResponse(404, "no such file", req.Method == "HEAD"), closeAfter), req.StreamID, req.Method, req.Path, t0)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return c.send(markClose(errorResponse(500, "cannot stat file", req.Method == "HEAD"), closeAfter), req.StreamID, req.Method, req.Path, t0)
	}
	size := info.Size()
	resp := &wire.Response{
		Status: 200,
		Headers: wire.Headers{
			{Name: "content-type", Value: contentType(path)},
			{Name: "content-length", Value: strconv.FormatInt(size, 10)},
			{Name: "last-modified", Value: info.ModTime().UTC().Format(httpDate)},
			{Name: "date", Value: time.Now().UTC().Format(httpDate)},
			{Name: "server", Value: ServerToken},
		},
	}
	markClose(resp, closeAfter)
	if req.Method == "HEAD" || size == 0 {
		return c.send(resp, req.StreamID, req.Method, req.Path, t0)
	}

	frames, err := wire.EncodeResponse(resp, req.StreamID, c.srv.cfg.MaxData)
	if err != nil {
		return err
	}
	head := frames[0]
	head.Flags &^= wire.FlagEndMessage
	if err := c.writeFrame(head); err != nil {
		return err
	}
	var sent int64
	for sent < size {
		n, err := io.ReadFull(f, c.chunk[:min(int64(len(c.chunk)), size-sent)])
		if err != nil {

			return fmt.Errorf("reading %s: %w", path, err)
		}
		sent += int64(n)
		data := wire.Frame{Type: wire.TypeData, StreamID: req.StreamID, Payload: c.chunk[:n]}
		if sent == size {
			data.Flags = wire.FlagEndMessage
		}
		if err := c.writeFrame(data); err != nil {
			return err
		}
	}
	c.logResponse(req.StreamID, req.Method, req.Path, 200, size, t0)
	return nil
}

func (c *conn) send(resp *wire.Response, id uint16, method, path string, t0 time.Time) error {
	frames, err := wire.EncodeResponse(resp, id, c.srv.cfg.MaxData)
	if err != nil {
		return err
	}
	for _, f := range frames {
		if err := c.writeFrame(f); err != nil {
			return err
		}
	}
	c.logResponse(id, method, path, resp.Status, int64(len(resp.Body)), t0)
	return nil
}

func (c *conn) writeFrame(f wire.Frame) error {
	c.nc.SetWriteDeadline(time.Now().Add(c.srv.cfg.WriteTimeout))
	c.frame = f.AppendTo(c.frame[:0])
	_, err := c.w.Write(c.frame)
	return err
}

func (c *conn) flush() error {
	if c.w.Buffered() == 0 {
		return nil
	}
	c.nc.SetWriteDeadline(time.Now().Add(c.srv.cfg.WriteTimeout))
	return c.w.Flush()
}

func (c *conn) logResponse(id uint16, method, path string, status int, n int64, t0 time.Time) {
	c.srv.answered.Add(1)
	c.srv.log.Info("response", "peer", c.peer, "stream", id, "method", method, "path", path,
		"status", status, "bytes", n, "ms", float64(time.Since(t0).Microseconds())/1000)
}

func (c *conn) lingerClose() {
	c.flush()
	if tc, ok := c.nc.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	c.nc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	io.CopyN(io.Discard, c.nc, 1<<20)
}

func markClose(resp *wire.Response, closeAfter bool) *wire.Response {
	if closeAfter {
		resp.Headers = append(resp.Headers, wire.Header{Name: "connection", Value: "close"})
	}
	return resp
}

func errorResponse(status int, reason string, head bool) *wire.Response {
	body := []byte(fmt.Sprintf("%d %s", status, reason))
	resp := &wire.Response{
		Status: status,
		Headers: wire.Headers{
			{Name: "content-type", Value: "text/plain; charset=utf-8"},
			{Name: "content-length", Value: strconv.Itoa(len(body))},
			{Name: "date", Value: time.Now().UTC().Format(httpDate)},
			{Name: "server", Value: ServerToken},
		},
	}
	if !head {
		resp.Body = body
	}
	return resp
}
