// Package lfsclean serves the clean half of git's long-running filter protocol
// for Git LFS files. It answers with the pointer git-lfs would write, hashing
// the stream as it arrives, so asking whether a tree's LFS file changed writes
// no temporary copy. It stores no object, so it serves only read-only git
// commands such as a cleanup proof's status.
package lfsclean

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"strconv"
	"strings"
)

const (
	pointerLimit  = 1024
	pointerHeader = "version https://"
	packetHeader  = 4
	maxPacket     = 65520
)

// Command is the filter.lfs.process value that runs this filter from the ccx
// binary at executable.
func Command(executable string) string {
	return "'" + strings.ReplaceAll(executable, "'", `'\''`) + "' vcs cleanup lfs-clean"
}

// Serve answers one git client until it closes in.
func Serve(in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	writer := bufio.NewWriter(out)
	if err := handshake(reader, writer); err != nil {
		return fmt.Errorf("lfs clean filter: %w", err)
	}
	for {
		request, err := readList(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lfs clean filter: %w", err)
		}
		if !slices.Contains(request, "command=clean") {
			return fmt.Errorf("lfs clean filter: serves only clean, got %q", request)
		}
		if err := answer(reader, writer); err != nil {
			return fmt.Errorf("lfs clean filter: %w", err)
		}
	}
}

func handshake(reader *bufio.Reader, writer *bufio.Writer) error {
	greeting, err := readList(reader)
	if err != nil {
		return fmt.Errorf("read the greeting: %w", err)
	}
	if !slices.Contains(greeting, "git-filter-client") || !slices.Contains(greeting, "version=2") {
		return fmt.Errorf("not a version 2 filter client: %q", greeting)
	}
	if err := writeList(writer, "git-filter-server", "version=2"); err != nil {
		return err
	}
	capabilities, err := readList(reader)
	if err != nil {
		return fmt.Errorf("read the capabilities: %w", err)
	}
	if !slices.Contains(capabilities, "capability=clean") {
		return fmt.Errorf("client offers no clean capability: %q", capabilities)
	}
	return writeList(writer, "capability=clean")
}

func answer(reader *bufio.Reader, writer *bufio.Writer) error {
	content := file{hash: sha256.New()}
	body := make([]byte, maxPacket)
	for {
		data, err := readPacket(reader, body)
		if err != nil {
			return fmt.Errorf("read the file: %w", err)
		}
		if data == nil {
			break
		}
		content.write(data)
	}
	if err := writeList(writer, "status=success"); err != nil {
		return err
	}
	if cleaned := content.cleaned(); len(cleaned) > 0 {
		if err := writePacket(writer, cleaned); err != nil {
			return err
		}
	}
	if err := writeFlush(writer); err != nil {
		return err
	}
	if err := writeFlush(writer); err != nil {
		return err
	}
	return writer.Flush()
}

type file struct {
	head []byte
	hash hash.Hash
	size int64
}

func (f *file) write(data []byte) {
	f.size += int64(len(data))
	f.hash.Write(data)
	if room := pointerLimit - len(f.head); room > 0 {
		f.head = append(f.head, data[:min(room, len(data))]...)
	}
}

func (f *file) cleaned() []byte {
	if f.size <= pointerLimit && (f.size == 0 || bytes.HasPrefix(bytes.TrimSpace(f.head), []byte(pointerHeader))) {
		return f.head
	}
	return fmt.Appendf(nil, "version https://git-lfs.github.com/spec/v1\noid sha256:%x\nsize %d\n", f.hash.Sum(nil), f.size)
}

func readList(reader *bufio.Reader) ([]string, error) {
	var lines []string
	body := make([]byte, maxPacket)
	for {
		data, err := readPacket(reader, body)
		if err != nil {
			return nil, err
		}
		if data == nil {
			return lines, nil
		}
		lines = append(lines, strings.TrimSuffix(string(data), "\n"))
	}
}

func readPacket(reader *bufio.Reader, body []byte) ([]byte, error) {
	var header [packetHeader]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	length, err := strconv.ParseUint(string(header[:]), 16, 16)
	if err != nil {
		return nil, fmt.Errorf("read a packet length: %w", err)
	}
	if length == 0 {
		return nil, nil
	}
	if length < packetHeader || length > maxPacket {
		return nil, fmt.Errorf("packet length %d", length)
	}
	data := body[:length-packetHeader]
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, fmt.Errorf("read a packet: %w", err)
	}
	return data, nil
}

func writeList(writer *bufio.Writer, lines ...string) error {
	for _, line := range lines {
		if err := writePacket(writer, []byte(line+"\n")); err != nil {
			return err
		}
	}
	if err := writeFlush(writer); err != nil {
		return err
	}
	return writer.Flush()
}

func writePacket(writer *bufio.Writer, data []byte) error {
	if _, err := fmt.Fprintf(writer, "%04x", len(data)+packetHeader); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}

func writeFlush(writer *bufio.Writer) error {
	_, err := writer.WriteString("0000")
	return err
}
