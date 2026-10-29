package vcs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func midxFixture(t *testing.T, version, hash byte, names ...string) []byte {
	t.Helper()
	if len(names) > math.MaxUint32 {
		t.Fatal("pack count exceeds the MIDX field range")
	}
	pnam := []byte(strings.Join(names, "\x00"))
	if len(names) > 0 {
		pnam = append(pnam, 0)
	}
	for len(pnam)%4 != 0 {
		pnam = append(pnam, 0)
	}
	header := []byte{'M', 'I', 'D', 'X', version, hash, 4, 0}
	header = binary.BigEndian.AppendUint32(header, uint32(len(names)))
	offset := uint64(12 + 5*12)
	for i, id := range []uint32{0x504e414d, 0x4f494446, 0x4f49444c, 0x4f4f4646, 0} {
		header = binary.BigEndian.AppendUint32(header, id)
		header = binary.BigEndian.AppendUint64(header, offset)
		switch i {
		case 0:
			offset += uint64(len(pnam))
		case 1:
			offset += 1024
		}
	}
	header = append(header, pnam...)
	hashSize := 20
	if hash == 2 {
		hashSize = 32
	}
	return append(header, make([]byte, 1024+hashSize)...)
}

type midxMetadataReader struct {
	data []byte
	end  int64
	read int
}

func (r *midxMetadataReader) ReadAt(p []byte, offset int64) (int, error) {
	if offset+int64(len(p)) > r.end {
		return 0, errors.New("read entered an object-data chunk")
	}
	n, err := bytes.NewReader(r.data).ReadAt(p, offset)
	r.read += n
	return n, err
}

func TestMIDXPackNamesReadOnlyMetadata(t *testing.T) {
	for _, version := range []byte{1, 2} {
		for _, hash := range []byte{1, 2} {
			t.Run(fmt.Sprintf("version%d-hash%d", version, hash), func(t *testing.T) {
				want := []string{"pack-a.idx", "pack-b.idx"}
				if version == 2 {
					slices.Reverse(want)
				}
				data := midxFixture(t, version, hash, want...)
				end := int64(len(data) - 1024 - 20)
				if hash == 2 {
					end -= 12
				}
				r := &midxMetadataReader{data: data, end: end}
				got, err := readMIDXPackNames(t.Context(), r, int64(len(data)))
				if err != nil || !slices.Equal(got, want) {
					t.Fatalf("pack names = %q, %v; want %q", got, err, want)
				}
				if r.read != int(end) {
					t.Fatalf("read %d metadata bytes, want %d", r.read, end)
				}
			})
		}
	}
}

func TestMIDXPackNamesRejectMalformedMetadata(t *testing.T) {
	tests := []struct {
		name string
		edit func([]byte) []byte
	}{
		{"truncated header", func(b []byte) []byte { return b[:8] }},
		{"signature", func(b []byte) []byte { b[0] = 'X'; return b }},
		{"version", func(b []byte) []byte { b[4] = 3; return b }},
		{"hash", func(b []byte) []byte { b[5] = 0; return b }},
		{"early terminator", func(b []byte) []byte { clear(b[12:16]); return b }},
		{"duplicate chunk", func(b []byte) []byte { copy(b[24:28], b[12:16]); return b }},
		{"unaligned chunk", func(b []byte) []byte { binary.BigEndian.PutUint64(b[16:], 73); return b }},
		{"chunk in header", func(b []byte) []byte { binary.BigEndian.PutUint64(b[16:], 12); return b }},
		{"out of bounds", func(b []byte) []byte { binary.BigEndian.PutUint64(b[28:], uint64(len(b))); return b }},
		{"missing names", func(b []byte) []byte { copy(b[12:16], "NONE"); return b }},
		{"excess count", func(b []byte) []byte { binary.BigEndian.PutUint32(b[8:], 100); return b }},
		{"unterminated name", func(b []byte) []byte { b[82] = 'x'; b[83] = 'x'; return b }},
		{"bad padding", func(b []byte) []byte { b[83] = 'x'; return b }},
		{"bad final entry", func(b []byte) []byte { b[60] = 'X'; return b }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.edit(midxFixture(t, 1, 1, "pack-a.idx"))
			if _, err := readMIDXPackNames(t.Context(), bytes.NewReader(b), int64(len(b))); err == nil {
				t.Fatal("malformed metadata accepted")
			}
		})
	}
	for _, names := range [][]string{{"pack-b.idx", "pack-a.idx"}, {"pack-a.idx", "pack-a.idx"}, {"../pack-a.idx"}, {"pack-a.pack"}} {
		b := midxFixture(t, 1, 1, names...)
		if _, err := readMIDXPackNames(t.Context(), bytes.NewReader(b), int64(len(b))); err == nil {
			t.Fatalf("invalid names accepted: %q", names)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readMIDXPackNames(ctx, bytes.NewReader(nil), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func writeMIDXFixture(t *testing.T, path string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, midxFixture(t, 1, 1, names...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCoveredPackIndexesFlatPrecedesChain(t *testing.T) {
	objectDir := t.TempDir()
	chainDir := filepath.Join(objectDir, "pack", "multi-pack-index.d")
	for i, name := range []string{"pack-a.idx", "pack-b.idx"} {
		hash := strings.Repeat(fmt.Sprint(i+1), 40)
		writeMIDXFixture(t, filepath.Join(chainDir, "multi-pack-index-"+hash+".midx"), name)
	}
	chain := strings.Repeat("1", 40) + "\n" + strings.Repeat("2", 40) + "\n"
	if err := os.WriteFile(filepath.Join(chainDir, "multi-pack-index-chain"), []byte(chain), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := coveredPackIndexes(t.Context(), objectDir)
	if err != nil || len(got) != 2 {
		t.Fatalf("chain coverage = %v, %v", got, err)
	}
	for _, name := range []string{"pack-a.idx", "pack-b.idx"} {
		if _, found := got[name]; !found {
			t.Fatalf("chain missing %s", name)
		}
	}
	writeMIDXFixture(t, filepath.Join(objectDir, "pack", "multi-pack-index"), "pack-flat.idx")
	got, err = coveredPackIndexes(t.Context(), objectDir)
	if err != nil || len(got) != 1 {
		t.Fatalf("flat coverage = %v, %v", got, err)
	}
	if _, found := got["pack-flat.idx"]; !found {
		t.Fatalf("flat index not preferred: %v", got)
	}
}

func TestCoveredPackIndexesActualGit(t *testing.T) {
	f := vcstest.Repo(t)
	runGit(t, f, f.Dir, "repack", "-ad")
	snapshot, err := CapturePackIndexes(f.Context(), render.Dir(f.Dir))
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, f, f.Dir, "multi-pack-index", "write", "--no-bitmap")
	covered, err := coveredPackIndexes(f.Context(), snapshot.objectDir)
	if err != nil || len(covered) != len(snapshot.indexes) || len(covered) == 0 {
		t.Fatalf("actual Git coverage = %v, %v; indexes %v", covered, err, snapshot.indexes)
	}
	for _, name := range snapshot.indexes {
		if _, found := covered[name]; !found {
			t.Fatalf("actual Git index missing %q", name)
		}
	}
}

func TestCoveredPackIndexesMissingAndBrokenChain(t *testing.T) {
	objectDir := t.TempDir()
	got, err := coveredPackIndexes(t.Context(), objectDir)
	if err != nil || len(got) != 0 {
		t.Fatalf("missing coverage = %v, %v", got, err)
	}
	chainDir := filepath.Join(objectDir, "pack", "multi-pack-index.d")
	if err := os.MkdirAll(chainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(chainDir, "multi-pack-index-chain")
	for _, chain := range []string{"invalid\n", strings.Repeat("z", 40) + "\n", strings.Repeat("1", 40) + "\n"} {
		if err := os.WriteFile(path, []byte(chain), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := coveredPackIndexes(t.Context(), objectDir); err == nil {
			t.Fatalf("broken chain accepted: %q", chain)
		}
	}
}

func TestPackIndexPublicationGeometricBytes(t *testing.T) {
	for _, tt := range []struct {
		name      string
		covered   int64
		uncovered int64
		want      bool
	}{
		{"below initial floor", 0, (8 << 20) - 1, false},
		{"initial floor", 0, 8 << 20, true},
		{"below fraction", 64 << 20, (16 << 20) - 1, false},
		{"fraction", 64 << 20, 16 << 20, true},
		{"floor dominates", 8 << 20, 2 << 20, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objectDir := t.TempDir()
			writePackPair(t, objectDir, "pack-covered")
			writePackPair(t, objectDir, "pack-uncovered")
			for name, size := range map[string]int64{"pack-covered.idx": tt.covered, "pack-uncovered.idx": tt.uncovered} {
				if err := os.Truncate(filepath.Join(objectDir, "pack", name), size); err != nil {
					t.Fatal(err)
				}
			}
			if tt.covered > 0 {
				writeMIDXFixture(t, filepath.Join(objectDir, "pack", "multi-pack-index"), "pack-covered.idx")
			}
			got, err := packIndexPublicationDue(t.Context(), objectDir, []string{"pack-covered.idx", "pack-uncovered.idx"})
			if err != nil || got != tt.want {
				t.Fatalf("due = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}
