package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/cleanup"
)

type server struct {
	engine   *Engine
	listener net.Listener
	lock     *durable.Lock

	closing      atomic.Bool
	shutdown     chan struct{}
	shutdownOnce sync.Once
	work         sync.WaitGroup

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

// Serve runs e behind layout's socket as the one daemon of that layout. It
// takes the serve lock before e reads its journal and holds it for its whole
// life, so a second Serve on the same layout fails on the lock having touched
// no record. It returns nil after ctx is done or a client asks for shutdown,
// once the engine has flushed and the socket is gone.
func Serve(ctx context.Context, e *Engine, layout cleanup.Layout) error {
	s, err := listen(ctx, e, layout)
	if err != nil {
		return err
	}
	return s.serve(ctx)
}

func listen(ctx context.Context, e *Engine, layout cleanup.Layout) (*server, error) {
	socket, err := layout.Socket()
	if err != nil {
		return nil, err
	}
	if err := layout.Ensure(); err != nil {
		return nil, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, cleanup.ServeLockWait)
	lock, err := durable.AcquireLock(lockCtx, layout.ServeLockPath())
	cancel()
	if err != nil {
		return nil, fmt.Errorf("cleanup daemon: take the serve lock %s: %w", layout.ServeLockPath(), err)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, errors.Join(fmt.Errorf("cleanup daemon: remove the stale socket: %w", err), lock.Close())
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("cleanup daemon: listen on %s: %w", socket, err), lock.Close())
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("cleanup daemon: make %s private: %w", socket, err), listener.Close(), lock.Close())
	}
	return &server{
		engine:   e,
		listener: listener,
		lock:     lock,
		shutdown: make(chan struct{}),
		conns:    make(map[net.Conn]struct{}),
	}, nil
}

func (s *server) serve(ctx context.Context) error {
	handlers, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	running := make(chan error, 1)
	go func() { running <- s.engine.Run(ctx) }()
	s.work.Go(func() { s.accept(handlers) })

	var (
		runErr  error
		stopped bool
	)
	select {
	case <-ctx.Done():
	case <-s.shutdown:
	case runErr = <-running:
		stopped = true
	}
	s.closing.Store(true)
	if !stopped {
		runErr = errors.Join(s.engine.Stop(handlers), <-running)
	}
	closeErr := s.listener.Close()
	cancel()
	s.closeConns()
	s.work.Wait()
	return errors.Join(runErr, closeErr, s.lock.Close())
}

func (s *server) accept(ctx context.Context) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Error("cleanup daemon: accept", "error", err)
				s.requestShutdown()
			}
			return
		}
		if s.closing.Load() || !s.track(conn) {
			_ = conn.Close()
			continue
		}
		s.work.Go(func() { s.handle(ctx, conn.(*net.UnixConn)) })
	}
}

func (s *server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *server) forget(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
	_ = conn.Close()
}

func (s *server) closeConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for conn := range s.conns {
		_ = conn.Close()
	}
}

func (s *server) requestShutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdown) })
}

func (s *server) handle(ctx context.Context, conn *net.UnixConn) {
	defer s.forget(conn)
	line, err := bufio.NewReader(io.LimitReader(conn, maxRequestBytes)).ReadBytes('\n')
	if err != nil {
		slog.Debug("cleanup daemon: a client sent no request", "error", err)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.work.Go(func() {
		_, _ = io.Copy(io.Discard, conn)
		cancel()
	})
	reply, shutdown := s.dispatch(ctx, conn, line)
	data, err := durable.Marshal(reply)
	if err != nil {
		data, err = durable.Marshal(response{Error: wireErrorOf(fmt.Errorf("cleanup daemon: encode reply: %w", err))})
	}
	if err == nil {
		_, err = conn.Write(data)
	}
	if err != nil {
		slog.Debug("cleanup daemon: a reply went undelivered", "error", err)
	}
	if shutdown {
		s.requestShutdown()
	}
}

func (s *server) dispatch(ctx context.Context, conn *net.UnixConn, line []byte) (response, bool) {
	req, err := durable.Unmarshal[request](line)
	if err != nil {
		var h head
		if json.Unmarshal(line, &h) == nil && h.Protocol != cleanup.Protocol {
			return incompatible(h.Protocol), false
		}
		return failure(fmt.Errorf("cleanup daemon: malformed request: %w", err)), false
	}
	switch {
	case req.Op == opHello:
		info := s.engine.info()
		return response{Info: &info}, false
	case req.Op == opShutdown:
		return response{}, true
	case req.Protocol != cleanup.Protocol:
		return incompatible(req.Protocol), false
	}
	switch req.Op {
	case opRemove, opDefer, opAdopt:
		pid, err := peerPID(conn)
		if err != nil {
			return failure(fmt.Errorf("cleanup daemon: identify the client behind the %s request: %w", req.Op, err)), false
		}
		return s.relocate(cleanup.WithRequester(ctx, pid), req), false
	}
	return s.execute(ctx, req), false
}

func (s *server) relocate(ctx context.Context, req request) response {
	var (
		receipt cleanup.Receipt
		err     error
	)
	switch req.Op {
	case opRemove:
		receipt, err = s.engine.Remove(ctx, *req.Remove)
	case opDefer:
		receipt, err = s.engine.Defer(ctx, *req.Defer)
	case opAdopt:
		receipt, err = s.engine.Adopt(ctx, *req.Adopt)
	}
	if err != nil {
		return failure(err)
	}
	return response{Receipt: &receipt}
}

func (s *server) execute(ctx context.Context, req request) response {
	switch req.Op {
	case opStatus:
		report, err := s.engine.Status(ctx, *req.Query)
		if err != nil {
			return failure(err)
		}
		return response{Report: &report}
	case opWait:
		job, err := s.engine.Wait(ctx, req.JobID)
		if err != nil {
			return failure(err)
		}
		return response{Job: &job}
	case opRetry:
		job, err := s.engine.Retry(ctx, req.JobID)
		if err != nil {
			return failure(err)
		}
		return response{Job: &job}
	case opPause:
		if err := s.engine.Pause(ctx); err != nil {
			return failure(err)
		}
	case opResume:
		if err := s.engine.Resume(ctx); err != nil {
			return failure(err)
		}
	}
	return response{}
}

func failure(err error) response { return response{Error: wireErrorOf(err)} }

func incompatible(sent int) response {
	return response{Error: &wireError{
		Kind:    kindIncompatible,
		Message: fmt.Sprintf("the daemon speaks protocol %d, the client sent %d", cleanup.Protocol, sent),
	}}
}
