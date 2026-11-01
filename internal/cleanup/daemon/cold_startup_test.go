package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func coldStartupOutput(t *testing.T, name string, result any) {
	t.Helper()
	root := os.Getenv("CCX_COLD_STARTUP_OUTPUT")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name+".json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupColdStartupReadiness(t *testing.T) {
	if os.Getenv("CCX_COLD_STARTUP_OUTPUT") == "" {
		t.Skip("the private cold-startup CI job retains this fixture's evidence")
	}
	root, err := os.MkdirTemp("", "ccxcold-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	h := newHarnessAt(t, root, paced())
	job := h.seed("cold", 1, cleanup.PhaseDone)
	hold := cleanup.Hold{ID: "cold-startup", Reason: "private readiness fixture", Since: h.clock.Now()}
	if err := h.journal.SaveSwitch(cleanup.Switch{Paused: true, Hold: &hold}); err != nil {
		t.Fatal(err)
	}
	e := h.build()
	e.mu.Lock()
	release := sync.OnceFunc(e.mu.Unlock)
	ctx, cancel := context.WithCancel(context.Background())
	s, err := listen(ctx, e, h.layout)
	if err != nil {
		release()
		cancel()
		t.Fatal(err)
	}
	socket, err := h.layout.Socket()
	if err != nil {
		release()
		cancel()
		t.Fatal(err)
	}
	f := &daemonFixture{t: t, h: h, socket: socket, client: Dial(socket), cancel: cancel, served: make(chan error, 1)}
	go func() { f.served <- s.serve(ctx) }()
	t.Cleanup(func() {
		release()
		cancel()
		if err := f.wait(); err != nil {
			t.Error(err)
		}
	})
	requestCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	info, err := f.client.Hello(requestCtx)
	if err != nil || info.Version != "v1.2.3" || info.Protocol != cleanup.Protocol || info.PID != os.Getpid() {
		t.Fatalf("Hello() = %+v, %v", info, err)
	}
	select {
	case <-e.loaded:
		t.Fatal("the held load published readiness")
	default:
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	data, err := durable.Marshal(request{Protocol: cleanup.Protocol, Version: info.Version, Op: opStatus, Query: &cleanup.Query{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err = bufio.NewReader(conn).ReadBytes('\n')
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("Status before readiness = %v, want the private read deadline", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	release()
	report, err := f.client.Status(requestCtx, cleanup.Query{})
	if err != nil || !report.Paused || len(report.Jobs) != 1 || report.Jobs[0].ID != job.ID || len(report.Damaged) != 0 {
		t.Fatalf("Status after readiness = %+v, %v", report, err)
	}
	state, err := f.client.HoldState(requestCtx)
	if err != nil || !state.Paused || state.Hold == nil || !sameHold(*state.Hold, hold) {
		t.Fatalf("HoldState after readiness = %+v, %v", state, err)
	}
	coldStartupOutput(t, "wire-readiness", map[string]any{"hello": info, "status_waited_for_loaded": true, "private_read_deadline_ns": int64(25 * time.Millisecond), "status_after_ready": report, "hold_after_ready": state, "controlled_gate": "engine load's publication mutex", "historical_wait_location_proven": false})
}

func TestCleanupColdJournalReplay(t *testing.T) {
	if os.Getenv("CCX_COLD_STARTUP_OUTPUT") == "" {
		t.Skip("the private cold-startup CI job retains this fixture's evidence")
	}
	for _, count := range []int{32, 7422} {
		t.Run(fmt.Sprintf("N%d", count), func(t *testing.T) {
			h := newHarness(t, paced())
			template := h.newJob("historical", 1, cleanup.PhaseDone)
			var bytes int64
			var seq uint64
			for i := range count {
				job := template
				seq++
				job.Seq = seq
				job.ID = fmt.Sprintf("%016x-%06x", h.clock.Now().UnixNano(), i+1)
				job.Registered, job.Payload = h.layout.Registered(job.ID), h.layout.Payload(job.ID)
				job.Updated = job.Updated.Add(time.Duration(i) * time.Nanosecond)
				data, err := durable.Marshal(job)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(h.layout.JobDir(job.ID), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(h.layout.RecordPath(job.ID), data, 0o600); err != nil {
					t.Fatal(err)
				}
				bytes += int64(len(data))
			}
			if err := h.journal.SaveSwitch(cleanup.Switch{Paused: true}); err != nil {
				t.Fatal(err)
			}
			e := h.build()
			started := time.Now()
			if err := e.load(); err != nil {
				t.Fatal(err)
			}
			load := time.Since(started)
			if len(e.jobs) != count || len(e.order) != count || e.seq != seq || len(e.damaged) != 0 {
				t.Fatalf("loaded %d jobs, %d order, seq %d, %d damaged", len(e.jobs), len(e.order), e.seq, len(e.damaged))
			}
			started = time.Now()
			report, err := e.Status(context.Background(), cleanup.Query{Limit: 1})
			status := time.Since(started)
			if err != nil || !report.Paused || len(report.Jobs) != 1 || report.Jobs[0].Seq != seq || report.Omitted != count-1 {
				t.Fatalf("Status() = %+v, %v", report, err)
			}
			coldStartupOutput(t, fmt.Sprintf("replay-%d", count), map[string]any{"records": count, "record_bytes": bytes, "engine_load_ns": int64(load), "first_status_ns": int64(status), "returned_seq": report.Jobs[0].Seq, "omitted": report.Omitted, "os_cache_flushed": false, "historical_population_or_latency_claimed": false})
		})
	}
}
