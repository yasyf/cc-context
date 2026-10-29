package vcs

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func coveredPackIndexes(ctx context.Context, objectDir string) (map[string]struct{}, error) {
	packDir := filepath.Join(objectDir, "pack")
	names, err := readMIDXFile(ctx, filepath.Join(packDir, "multi-pack-index"))
	if err == nil {
		return packNameSet(names), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	chainDir := filepath.Join(packDir, "multi-pack-index.d")
	chain, err := os.Open(filepath.Join(chainDir, "multi-pack-index-chain"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open multi-pack index chain: %w", err)
	}
	defer chain.Close()
	covered := make(map[string]struct{})
	scanner := bufio.NewScanner(chain)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hash := scanner.Text()
		if len(hash) != 40 && len(hash) != 64 {
			return nil, fmt.Errorf("invalid multi-pack index chain hash %q", hash)
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return nil, fmt.Errorf("decode multi-pack index chain hash: %w", err)
		}
		names, err := readMIDXFile(ctx, filepath.Join(chainDir, "multi-pack-index-"+hash+".midx"))
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			covered[name] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read multi-pack index chain: %w", err)
	}
	return covered, nil
}

func packNameSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}

func readMIDXFile(ctx context.Context, path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open multi-pack index: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat multi-pack index: %w", err)
	}
	return readMIDXPackNames(ctx, f, info.Size())
}

func readMIDXPackNames(ctx context.Context, r io.ReaderAt, size int64) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	header := make([]byte, 12)
	if _, err := r.ReadAt(header, 0); err != nil {
		return nil, fmt.Errorf("read multi-pack index header: %w", err)
	}
	if string(header[:4]) != "MIDX" || header[4] < 1 || header[4] > 2 || header[5] < 1 || header[5] > 2 {
		return nil, errors.New("invalid multi-pack index header")
	}
	hashSize := int64(20)
	if header[5] == 2 {
		hashSize = 32
	}
	chunks := int(header[6])
	table := make([]byte, (chunks+1)*12)
	if _, err := r.ReadAt(table, 12); err != nil {
		return nil, fmt.Errorf("read multi-pack index chunks: %w", err)
	}
	seen := make(map[uint32]struct{}, chunks)
	var start, length int64
	for i := range chunks {
		entry := table[i*12:]
		id := binary.BigEndian.Uint32(entry)
		offset := binary.BigEndian.Uint64(entry[4:])
		next := binary.BigEndian.Uint64(entry[16:])
		_, duplicate := seen[id]
		if id == 0 || duplicate || offset < uint64(12+len(table)) || offset%4 != 0 || next < offset || size < hashSize || next > uint64(size-hashSize) {
			return nil, errors.New("invalid multi-pack index chunk table")
		}
		seen[id] = struct{}{}
		if id == 0x504e414d {
			start, length = int64(offset), int64(next-offset)
		}
	}
	if binary.BigEndian.Uint32(table[chunks*12:]) != 0 || start == 0 {
		return nil, errors.New("missing multi-pack index pack-name chunk")
	}
	count := binary.BigEndian.Uint32(header[8:])
	if uint64(count) > uint64(length)/2 {
		return nil, errors.New("short multi-pack index pack-name chunk")
	}
	reader := bufio.NewReader(io.NewSectionReader(r, start, length))
	names := make([]string, 0, count)
	unique := make(map[string]struct{}, count)
	for range count {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name, err := reader.ReadString(0)
		if err != nil {
			return nil, fmt.Errorf("read multi-pack index pack name: %w", err)
		}
		name = strings.TrimSuffix(name, "\x00")
		_, duplicate := unique[name]
		if filepath.Base(name) != name || !strings.HasSuffix(name, ".idx") || duplicate || (header[4] == 1 && len(names) > 0 && names[len(names)-1] >= name) {
			return nil, fmt.Errorf("invalid multi-pack index pack name %q", name)
		}
		unique[name] = struct{}{}
		names = append(names, name)
	}
	padding, err := io.ReadAll(io.LimitReader(reader, 4))
	if err != nil {
		return nil, fmt.Errorf("read multi-pack index padding: %w", err)
	}
	if len(padding) > 3 || strings.Trim(string(padding), "\x00") != "" {
		return nil, errors.New("invalid multi-pack index pack-name padding")
	}
	return names, nil
}

func packIndexPublicationDue(ctx context.Context, objectDir string, indexes []string) (bool, error) {
	covered, err := coveredPackIndexes(ctx, objectDir)
	if err != nil {
		return false, err
	}
	var coveredBytes, uncoveredBytes int64
	for _, name := range indexes {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		info, err := os.Stat(filepath.Join(objectDir, "pack", name))
		if err != nil {
			return false, fmt.Errorf("stat pack index %q: %w", name, err)
		}
		if _, found := covered[name]; found {
			coveredBytes += info.Size()
		} else {
			uncoveredBytes += info.Size()
		}
	}
	return uncoveredBytes >= max(coveredBytes/4, 8<<20), nil
}
