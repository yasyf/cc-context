package embed

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- E3: ABI bounds guard + free-on-fail ------------------------------------

// fakeBlobWriter fakes the guest-memory boundary so writeBlob's failure path is
// exercised without a multi-GiB allocation: it records alloc/free calls and can
// fail the copy on demand.
type fakeBlobWriter struct {
	ptr        uint32
	writeOK    bool
	allocCalls int
	freed      []freeCall
}

type freeCall struct{ ptr, n uint32 }

func (f *fakeBlobWriter) guestAlloc(_ context.Context, _ uint32) (uint32, error) {
	f.allocCalls++
	return f.ptr, nil
}

func (f *fakeBlobWriter) guestWrite(_ uint32, _ []byte) bool { return f.writeOK }

func (f *fakeBlobWriter) guestFree(_ context.Context, ptr, n uint32) {
	f.freed = append(f.freed, freeCall{ptr, n})
}

func TestWriteBlobFreesOnWriteFailure(t *testing.T) {
	f := &fakeBlobWriter{ptr: 4242, writeOK: false}
	data := []byte("payload bytes")

	_, err := writeBlob(context.Background(), f, data)
	if err == nil {
		t.Fatal("writeBlob: want error on write failure, got nil")
	}
	if f.allocCalls != 1 {
		t.Fatalf("allocCalls = %d, want 1", f.allocCalls)
	}
	if len(f.freed) != 1 || f.freed[0].ptr != 4242 || int(f.freed[0].n) != len(data) {
		t.Fatalf("freed = %+v, want exactly [{ptr:4242 n:%d}] (failed write must release its allocation)", f.freed, len(data))
	}
}

func TestWriteBlobNoFreeOnSuccess(t *testing.T) {
	f := &fakeBlobWriter{ptr: 4242, writeOK: true}

	ptr, err := writeBlob(context.Background(), f, []byte("payload"))
	if err != nil {
		t.Fatalf("writeBlob: %v", err)
	}
	if ptr != 4242 {
		t.Fatalf("ptr = %d, want 4242", ptr)
	}
	if len(f.freed) != 0 {
		t.Fatalf("freed = %+v, want none on success", f.freed)
	}
}

func TestWriteBlobRejectsOverU32(t *testing.T) {
	orig := maxU32Bytes
	defer func() { maxU32Bytes = orig }()
	maxU32Bytes = 4

	f := &fakeBlobWriter{ptr: 1, writeOK: true}
	_, err := writeBlob(context.Background(), f, []byte("over the limit"))
	if err == nil || !strings.Contains(err.Error(), "u32 linear-address limit") {
		t.Fatalf("writeBlob over-u32 err = %v, want u32 limit rejection", err)
	}
	if f.allocCalls != 0 {
		t.Fatalf("allocCalls = %d, want 0 — an over-u32 blob must not allocate", f.allocCalls)
	}
}

func TestFrameBatchBounds(t *testing.T) {
	orig := maxU32Bytes
	defer func() { maxU32Bytes = orig }()

	tests := []struct {
		name    string
		limit   uint64
		texts   []string
		wantErr string
	}{
		{"count over limit", 2, []string{"", "", ""}, "frame-count"},
		{"text over limit", 3, []string{"hello"}, "frame-length"},
		{"total over limit", 10, []string{"aaaa", "bbbb"}, "u32 address limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maxU32Bytes = tt.limit
			buf, err := frameBatch(tt.texts)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("frameBatch(%q) err = %v, want containing %q", tt.texts, err, tt.wantErr)
			}
			if buf != nil {
				t.Fatalf("frameBatch(%q) returned a %d-byte buffer on rejection, want nil", tt.texts, len(buf))
			}
		})
	}
}

func TestFrameBatchRoundTrip(t *testing.T) {
	buf, err := frameBatch([]string{"ab", "c"})
	if err != nil {
		t.Fatalf("frameBatch: %v", err)
	}
	// [u32 count=2][u32 len=2]"ab"[u32 len=1]"c"
	if want := 4 + (4 + 2) + (4 + 1); len(buf) != want {
		t.Fatalf("frame is %d bytes, want %d", len(buf), want)
	}
	if got := binary.LittleEndian.Uint32(buf[0:4]); got != 2 {
		t.Fatalf("frame count = %d, want 2", got)
	}
}

// --- E2: Close releases the cache and is idempotent -------------------------

func TestCloseIdempotentReleasesHandles(t *testing.T) {
	eng, err := New(context.Background())
	if errors.Is(err, ErrWeightsUnavailable) {
		t.Skip("model weights unavailable (offline, empty cache) — skipping")
	}
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := eng.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Retained handles are nulled so a closed Engine pins no model memory.
	if eng.runtime != nil || eng.cache != nil || eng.compiled != nil || eng.module != nil {
		t.Fatalf("Close left handles set: runtime=%v cache=%v compiled=%v module=%v",
			eng.runtime != nil, eng.cache != nil, eng.compiled != nil, eng.module != nil)
	}
	// Idempotent: a second Close is a no-op, not a panic or error.
	if err := eng.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// --- E4: weight download is size-bounded before verification ----------------

func TestDownloadBounded(t *testing.T) {
	orig := maxWeightFileBytes
	defer func() { maxWeightFileBytes = orig }()
	maxWeightFileBytes = 1024

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 4096)) // exceeds the 1024-byte cap
	}))
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)

	_, err := download(context.Background(), weightFile{name: "config.json", sha256: "unused"})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("download over-cap err = %v, want bounded-read rejection", err)
	}
	// Oversized body is a broken mirror, a hard error — never the skippable
	// offline sentinel, so callers do not treat it as "no network".
	if errors.Is(err, ErrWeightsUnavailable) {
		t.Fatalf("over-cap download wrongly reported as ErrWeightsUnavailable: %v", err)
	}
}

func TestDownloadNormalVerifies(t *testing.T) {
	body := []byte(`{"hidden_size":256}`)
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)

	got, err := download(context.Background(), weightFile{name: "config.json", sha256: want})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("download body = %q, want %q", got, body)
	}
	if err := verifyChecksum(got, want); err != nil {
		t.Fatalf("verifyChecksum on a normal-size body: %v", err)
	}
}
