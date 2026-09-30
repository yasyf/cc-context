package cleanupwatch

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

const lstartFields = 5

// ProcTable is the production [Processes]: one ps listing filtered to the
// builtin fsmonitor daemons, then one lsof bounded to at most MaxDaemons of
// those pids.
type ProcTable struct {
	Run        Runner
	Timeout    time.Duration
	MaxDaemons int
}

// FSMonitorDaemons lists every `git fsmonitor--daemon run` process with its
// start time, working directory, and bound unix-domain sockets. Daemons past
// MaxDaemons are listed Unexamined, without either.
func (p ProcTable) FSMonitorDaemons(ctx context.Context) ([]Process, error) {
	out, err := p.run(ctx, "ps", "-axo", "pid=,lstart=,command=")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	var daemons []Process
	for _, proc := range parsePS(out) {
		if isFSMonitorDaemon(proc.Command) {
			daemons = append(daemons, proc)
		}
	}
	if len(daemons) == 0 {
		return nil, nil
	}
	examined := min(len(daemons), p.MaxDaemons)
	for i := examined; i < len(daemons); i++ {
		daemons[i].Unexamined = true
	}
	pids := make([]string, examined)
	for i, d := range daemons[:examined] {
		pids[i] = strconv.Itoa(d.PID)
	}
	out, err = p.run(ctx, "lsof", "-n", "-P", "-w", "-F", "ftn", "-p", strings.Join(pids, ","))
	if exitCode(err) == 1 {
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("list fsmonitor sockets: %w", err)
	}
	files := parseLsof(out)
	var unread []int
	for _, d := range daemons[:examined] {
		if _, ok := files[d.PID]; !ok {
			unread = append(unread, d.PID)
		}
	}
	alive, err := p.Processes(ctx, unread)
	if err != nil {
		return nil, err
	}
	live := daemons[:0]
	for _, d := range daemons {
		f, read := files[d.PID]
		if now, ok := alive[d.PID]; !d.Unexamined && !read && (!ok || now.Start != d.Start) {
			continue
		}
		d.CWD, d.Sockets = f.CWD, f.Sockets
		live = append(live, d)
	}
	return live, nil
}

// Processes returns the start time and command line of every pid in pids that
// is still running, in one ps call.
func (p ProcTable) Processes(ctx context.Context, pids []int) (map[int]Process, error) {
	if len(pids) == 0 {
		return map[int]Process{}, nil
	}
	list := make([]string, len(pids))
	for i, pid := range pids {
		list[i] = strconv.Itoa(pid)
	}
	out, err := p.run(ctx, "ps", "-o", "pid=,lstart=,command=", "-p", strings.Join(list, ","))
	if exitCode(err) == 1 && len(out) == 0 {
		return map[int]Process{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect pids %s: %w", strings.Join(list, ","), err)
	}
	procs := map[int]Process{}
	for _, proc := range parsePS(out) {
		if slices.Contains(pids, proc.PID) {
			procs[proc.PID] = proc
		}
	}
	return procs, nil
}

func (p ProcTable) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	out, err := p.Run.Run(ctx, render.Ambient, name, args...)
	return out.Stdout, err
}

func parsePS(out []byte) []Process {
	var procs []Process
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields, command := cutFields(sc.Text(), 1+lstartFields)
		if len(fields) < 1+lstartFields || command == "" {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		procs = append(procs, Process{PID: pid, Start: strings.Join(fields[1:], " "), Command: command})
	}
	return procs
}

func cutFields(line string, n int) ([]string, string) {
	var fields []string
	rest := line
	for len(fields) < n {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" {
			return fields, ""
		}
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			return append(fields, rest), ""
		}
		fields = append(fields, rest[:end])
		rest = rest[end:]
	}
	return fields, strings.TrimSpace(rest)
}

func isFSMonitorDaemon(command string) bool {
	f := strings.Fields(command)
	if len(f) < 3 || filepath.Base(f[0]) != "git" {
		return false
	}
	i := slices.Index(f[1:], "fsmonitor--daemon") + 1
	return i > 0 && i+1 < len(f) && f[i+1] == "run"
}

type openFiles struct {
	CWD     string
	Sockets []string
}

func parseLsof(out []byte) map[int]openFiles {
	files := map[int]openFiles{}
	pid, fd, typ := 0, "", ""
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		field, value := line[0], line[1:]
		switch field {
		case 'p':
			pid, _ = strconv.Atoi(value)
			files[pid] = openFiles{}
			fd, typ = "", ""
		case 'f':
			fd, typ = value, ""
		case 't':
			typ = value
		case 'n':
			f := files[pid]
			switch {
			case fd == "cwd":
				f.CWD = value
			case typ == "unix" && !strings.HasPrefix(value, "->"):
				f.Sockets = append(f.Sockets, value)
			}
			files[pid] = f
		}
	}
	return files
}
