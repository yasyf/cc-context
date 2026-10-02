package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"syscall"
	"time"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/version"
)

const (
	shutdownPoll    = 10 * time.Millisecond
	shutdownPollMax = 200 * time.Millisecond
)

// Client reaches a serving daemon over its socket, one connection per call.
// The errors it returns are the ones the engine returned, rebuilt so
// errors.As and errors.Is match exactly as they do in process.
type Client struct {
	socket string
}

var (
	_ cleanup.Control = (*Client)(nil)
	_ cleanup.Adopter = (*Client)(nil)
)

// Dial returns a Client for the daemon at socket. Nothing connects until a
// method is called.
func Dial(socket string) *Client { return &Client{socket: socket} }

// Hello identifies the serving daemon. It is answered whatever protocol the
// daemon speaks, so a client can tell an older daemon from a newer one.
func (c *Client) Hello(ctx context.Context) (cleanup.Info, error) {
	reply, err := c.roundTrip(ctx, request{Op: opHello})
	return hello(reply, err)
}

// Greet is Hello on one connection whose kernel peer observe receives before
// the hello is sent, so what observe records belongs to the process whose
// reply follows.
func (c *Client) Greet(ctx context.Context, observe func(cleanup.Peer)) (cleanup.Info, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return cleanup.Info{}, err
	}
	peer, err := peerCred(conn)
	if err != nil {
		_ = conn.Close()
		return cleanup.Info{}, fmt.Errorf("%w: %s: %w", cleanup.ErrUnidentifiedPeer, c.socket, err)
	}
	observe(peer)
	reply, err := send(ctx, conn, request{Op: opHello})
	return hello(reply, err)
}

func hello(reply response, err error) (cleanup.Info, error) {
	if err != nil {
		return cleanup.Info{}, err
	}
	if reply.Info == nil {
		return cleanup.Info{}, errors.New("cleanup daemon: the hello reply carries no info")
	}
	return *reply.Info, nil
}

// Shutdown sends the stop on one connection whose peer verify accepts, then
// returns once that process no longer serves the socket: it is gone, refuses
// connections, or another process answers. Other connect failures are returned.
func (c *Client) Shutdown(ctx context.Context, verify func(cleanup.Peer) error) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	peer, err := peerCred(conn)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("%w: %s: %w", cleanup.ErrUnidentifiedPeer, c.socket, err)
	}
	if err := verify(peer); err != nil {
		_ = conn.Close()
		return err
	}
	if _, err := send(ctx, conn, request{Op: opShutdown}); err != nil {
		return err
	}
	for wait := shutdownPoll; ; wait = min(wait*2, shutdownPollMax) {
		conn, err := c.connect(ctx)
		if err == nil {
			serving, peerErr := peerPID(conn)
			_ = conn.Close()
			if peerErr == nil && serving != peer.PID {
				return nil
			}
		}
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED):
			return nil
		case err != nil:
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// Remove relocates and unregisters a worktree. The daemon finishes the ladder
// step it is on even when this call is abandoned.
func (c *Client) Remove(ctx context.Context, r cleanup.Request) (cleanup.Receipt, error) {
	return c.receipt(ctx, request{Op: opRemove, Remove: &r})
}

// Defer journals a removal that waits for inactivity.
func (c *Client) Defer(ctx context.Context, r cleanup.DeferRequest) (cleanup.Receipt, error) {
	return c.receipt(ctx, request{Op: opDefer, Defer: &r})
}

// Adopt hands the daemon one tree the retired janitor parked in its
// quarantine.
func (c *Client) Adopt(ctx context.Context, r cleanup.AdoptRequest) (cleanup.Receipt, error) {
	return c.receipt(ctx, request{Op: opAdopt, Adopt: &r})
}

// Status reports the queue, or one job.
func (c *Client) Status(ctx context.Context, q cleanup.Query) (cleanup.Report, error) {
	reply, err := c.roundTrip(ctx, request{Op: opStatus, Query: &q})
	if err != nil {
		return cleanup.Report{}, err
	}
	if reply.Report == nil {
		return cleanup.Report{}, errors.New("cleanup daemon: the status reply carries no report")
	}
	return *reply.Report, nil
}

// Wait holds its connection open until the job is done, returning the job
// with a *cleanup.BlockedError once it blocks.
func (c *Client) Wait(ctx context.Context, jobID string) (cleanup.Job, error) {
	job, err := c.job(ctx, request{Op: opWait, JobID: jobID})
	if blocked := (*cleanup.BlockedError)(nil); errors.As(err, &blocked) {
		return blocked.Job, err
	}
	return job, err
}

// Pause stops physical deletion queue-wide.
func (c *Client) Pause(ctx context.Context) error {
	_, err := c.roundTrip(ctx, request{Op: opPause})
	return err
}

// Resume lets physical deletion continue.
func (c *Client) Resume(ctx context.Context) error {
	_, err := c.roundTrip(ctx, request{Op: opResume})
	return err
}

// Retry clears a job's blockage and returns the job as journaled.
func (c *Client) Retry(ctx context.Context, jobID string) (cleanup.Job, error) {
	return c.job(ctx, request{Op: opRetry, JobID: jobID})
}

func (c *Client) receipt(ctx context.Context, req request) (cleanup.Receipt, error) {
	reply, err := c.roundTrip(ctx, req)
	if err != nil {
		return cleanup.Receipt{}, err
	}
	if reply.Receipt == nil {
		return cleanup.Receipt{}, fmt.Errorf("cleanup daemon: the %s reply carries no receipt", req.Op)
	}
	return *reply.Receipt, nil
}

func (c *Client) job(ctx context.Context, req request) (cleanup.Job, error) {
	reply, err := c.roundTrip(ctx, req)
	if err != nil {
		return cleanup.Job{}, err
	}
	if reply.Job == nil {
		return cleanup.Job{}, fmt.Errorf("cleanup daemon: the %s reply carries no job", req.Op)
	}
	return *reply.Job, nil
}

func (c *Client) connect(ctx context.Context) (*net.UnixConn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, fmt.Errorf("cleanup daemon: connect to %s: %w", c.socket, err)
	}
	return conn.(*net.UnixConn), nil
}

func (c *Client) roundTrip(ctx context.Context, req request) (response, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return response{}, err
	}
	return send(ctx, conn, req)
}

func send(ctx context.Context, conn net.Conn, req request) (response, error) {
	defer func() { _ = conn.Close() }()
	defer context.AfterFunc(ctx, func() { _ = conn.Close() })()
	req.Protocol, req.Version = cleanup.Protocol, version.String()
	data, err := durable.Marshal(req)
	if err != nil {
		return response{}, fmt.Errorf("cleanup daemon: encode the %s request: %w", req.Op, err)
	}
	line, err := exchange(conn, data)
	if ctx.Err() != nil {
		return response{}, ctx.Err()
	}
	if err != nil {
		return response{}, fmt.Errorf("cleanup daemon: %s: %w", req.Op, err)
	}
	reply, err := durable.Unmarshal[response](line)
	if err != nil {
		return response{}, fmt.Errorf("cleanup daemon: decode the %s reply: %w", req.Op, err)
	}
	if reply.Error != nil {
		return response{}, reply.Error.rebuild()
	}
	return reply, nil
}

func exchange(conn net.Conn, data []byte) ([]byte, error) {
	if _, err := conn.Write(data); err != nil {
		return nil, err
	}
	return bufio.NewReader(io.LimitReader(conn, maxReplyBytes)).ReadBytes('\n')
}
