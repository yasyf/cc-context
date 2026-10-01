package cleanupwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

type watchmanStatus struct {
	Roots     []rawRoot   `json:"roots"`
	Clients   []rawClient `json:"clients"`
	SelfPID   int         `json:"-"`
	ServerPID int         `json:"-"`
}

type rawRoot struct {
	Path        string `json:"path"`
	FSType      string `json:"fstype"`
	Watcher     string `json:"watcher"`
	Uptime      int64  `json:"uptime"`
	DoneInitial bool   `json:"done_initial"`
	Recrawl     struct {
		Count         int     `json:"count"`
		ShouldRecrawl bool    `json:"should-recrawl"`
		Warning       *string `json:"warning"`
		Reason        string  `json:"reason"`
		Started       *int64  `json:"started"`
		Completed     *int64  `json:"completed"`
	} `json:"recrawl_info"`
	Queries []rawQuery `json:"queries"`
}

type rawQuery struct {
	State     string `json:"state"`
	ClientPID int    `json:"client-pid"`
	Elapsed   int64  `json:"elapsed-milliseconds"`
}

type rawClient struct {
	State string `json:"state"`
	Peer  *struct {
		PID  int    `json:"pid"`
		Name string `json:"name"`
	} `json:"peer"`
}

type rawSubscriptions struct {
	Subscribers []struct {
		Info *struct {
			Name   string `json:"name"`
			Client string `json:"client"`
			PID    int    `json:"pid"`
		} `json:"info"`
	} `json:"subscribers"`
	Subscriptions []struct {
		Name     string `json:"name"`
		ClientID int    `json:"client_id"`
	} `json:"subscriptions"`
}

type rawTrigger struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
}

type rootConsumers struct {
	Subscriptions []Subscription
	Triggers      []Trigger
}

// WatchmanError is an error response from the Watchman server.
type WatchmanError struct {
	Command string
	Message string
}

func (e *WatchmanError) Error() string {
	return fmt.Sprintf("watchman %s: %s", e.Command, e.Message)
}

func (d Deps) watchman(ctx context.Context, v any, args ...string) error {
	_, err := d.watchmanPID(ctx, v, args...)
	return err
}

func (d Deps) watchmanPID(ctx context.Context, v any, args ...string) (int, error) {
	res, err := d.runPID(ctx, render.Ambient, "watchman", append([]string{"--no-spawn", "--no-pretty"}, args...)...)
	out := res.Stdout
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		out = exitErr.Stdout
	}
	var resp struct {
		Error string `json:"error"`
	}
	if jerr := json.Unmarshal(out, &resp); jerr == nil && resp.Error != "" {
		return 0, &WatchmanError{Command: args[0], Message: resp.Error}
	}
	if err != nil {
		return 0, fmt.Errorf("watchman %s: %w", args[0], err)
	}
	if err := json.Unmarshal(out, v); err != nil {
		return 0, fmt.Errorf("watchman %s: decode: %w", args[0], err)
	}
	return res.PID, nil
}

type server struct {
	Version string
	PID     int
	Absent  string
}

func (d Deps) probe(ctx context.Context) (server, error) {
	var sock struct {
		Sockname string `json:"sockname"`
		Version  string `json:"version"`
	}
	err := d.watchman(ctx, &sock, "get-sockname")
	if errors.Is(err, exec.ErrNotFound) {
		return server{Absent: "watchman is not installed"}, nil
	}
	if err != nil {
		return server{}, err
	}
	var pid struct {
		PID int `json:"pid"`
	}
	err = d.watchman(ctx, &pid, "get-pid")
	if err == nil {
		return server{Version: sock.Version, PID: pid.PID}, nil
	}
	down, derr := serverDown(sock.Sockname)
	if derr != nil {
		return server{}, errors.Join(err, derr)
	}
	if !down {
		return server{}, err
	}
	return server{Version: sock.Version, Absent: "watchman server is not running"}, nil
}

func serverDown(sock string) (bool, error) {
	if _, err := os.Lstat(sock); errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err == nil {
		return false, conn.Close()
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	return false, fmt.Errorf("probe watchman socket %s: %w", sock, err)
}

func (d Deps) watchList(ctx context.Context) ([]string, error) {
	var resp struct {
		Roots []string `json:"roots"`
	}
	if err := d.watchman(ctx, &resp, "watch-list"); err != nil {
		return nil, err
	}
	slices.Sort(resp.Roots)
	return resp.Roots, nil
}

func (d Deps) debugStatus(ctx context.Context) (watchmanStatus, error) {
	var resp watchmanStatus
	pid, err := d.watchmanPID(ctx, &resp, "debug-status")
	if err != nil {
		return watchmanStatus{}, err
	}
	resp.SelfPID = pid
	slices.SortFunc(resp.Roots, func(a, b rawRoot) int { return strings.Compare(a.Path, b.Path) })
	return resp, nil
}

func (d Deps) consumers(ctx context.Context, root string) (rootConsumers, error) {
	var subs rawSubscriptions
	if err := d.watchman(ctx, &subs, "debug-get-subscriptions", root); err != nil {
		return rootConsumers{}, err
	}
	var trig struct {
		Triggers []rawTrigger `json:"triggers"`
	}
	if err := d.watchman(ctx, &trig, "trigger-list", root); err != nil {
		return rootConsumers{}, err
	}
	c := rootConsumers{Subscriptions: mergeSubscriptions(subs), Triggers: []Trigger{}}
	for _, t := range trig.Triggers {
		c.Triggers = append(c.Triggers, Trigger(t))
	}
	return c, nil
}

func mergeSubscriptions(raw rawSubscriptions) []Subscription {
	subs := []Subscription{}
	index := map[string]int{}
	for _, s := range raw.Subscriptions {
		client := strconv.Itoa(s.ClientID)
		index[s.Name+"\x00"+client] = len(subs)
		subs = append(subs, Subscription{Name: s.Name, Client: client})
	}
	for _, s := range raw.Subscribers {
		if s.Info == nil {
			subs = append(subs, Subscription{})
			continue
		}
		if i, ok := index[s.Info.Name+"\x00"+s.Info.Client]; ok {
			subs[i].PID = s.Info.PID
			continue
		}
		subs = append(subs, Subscription{Name: s.Info.Name, PID: s.Info.PID, Client: s.Info.Client})
	}
	return subs
}

func (d Deps) loadedConfig(ctx context.Context, root string) (json.RawMessage, error) {
	var resp struct {
		Config json.RawMessage `json:"config"`
	}
	if err := d.watchman(ctx, &resp, "get-config", root); err != nil {
		return nil, err
	}
	if len(resp.Config) == 0 {
		return json.RawMessage("{}"), nil
	}
	return resp.Config, nil
}

func (d Deps) watchDel(ctx context.Context, root string) error {
	var resp struct {
		Deleted bool   `json:"watch-del"`
		Root    string `json:"root"`
	}
	if err := d.watchman(ctx, &resp, "watch-del", root); err != nil {
		return err
	}
	if !resp.Deleted || resp.Root != root {
		return fmt.Errorf("watchman watch-del %s: server answered watch-del=%t root=%q", root, resp.Deleted, resp.Root)
	}
	return nil
}

func (d Deps) watch(ctx context.Context, root string) error {
	var resp struct {
		Watch string `json:"watch"`
	}
	if err := d.watchman(ctx, &resp, "watch", root); err != nil {
		return err
	}
	if resp.Watch != root {
		return fmt.Errorf("watchman watch %s: server watched %q", root, resp.Watch)
	}
	return nil
}

type configSnapshot struct {
	Raw     json.RawMessage
	Present bool
	Dev     uint64
	Ino     uint64
	Error   string
}

func snapshotConfig(root string) (snap configSnapshot) {
	f, err := os.Open(filepath.Join(root, ".watchmanconfig")) //nolint:gosec // root is a watch root Watchman itself reported; .watchmanconfig is its fixed config file
	if errors.Is(err, fs.ErrNotExist) {
		return configSnapshot{Raw: json.RawMessage("{}")}
	}
	if err != nil {
		return configSnapshot{Error: err.Error()}
	}
	defer func() {
		if err := f.Close(); err != nil && snap.Error == "" {
			snap.Error = fmt.Sprintf("close .watchmanconfig: %v", err)
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		return configSnapshot{Error: err.Error()}
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return configSnapshot{Error: "stat .watchmanconfig: no device and inode"}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return configSnapshot{Error: err.Error()}
	}
	snap = configSnapshot{Raw: json.RawMessage(b), Present: true, Dev: uint64(st.Dev), Ino: st.Ino} //nolint:gosec // Stat_t.Dev is int32 on darwin; the same conversion on both sides keeps identities comparable
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		snap.Error = fmt.Sprintf("parse .watchmanconfig: %v", err)
	}
	return snap
}

func (s configSnapshot) same(o configSnapshot) bool {
	return s.Present == o.Present && s.Dev == o.Dev && s.Ino == o.Ino && s.Error == o.Error && bytes.Equal(s.Raw, o.Raw)
}

func sameConfig(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

func ignoreDirs(config json.RawMessage) []string {
	var c struct {
		IgnoreDirs []string `json:"ignore_dirs"`
	}
	if json.Unmarshal(config, &c) != nil {
		return nil
	}
	return c.IgnoreDirs
}
