package read

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func readNumeric(path, section string, start, end int, rangeErr error) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path is the caller's own read target, validated upstream
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	lines, total, err := numericWindow(f, start, end)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if total == 0 {
		return fmt.Sprintf("# read %s: empty file\n", path), nil
	}
	if rangeErr != nil {
		return "", fmt.Errorf("read %s: %w", path, rangeErr)
	}
	start, end, err = numericBounds(section, path, start, end, total)
	if err != nil {
		return "", err
	}
	return render(path, lines, start, end, total, false), nil
}

func numericBounds(section, path string, start, end, total int) (int, int, error) {
	if start < 1 {
		return 0, 0, fmt.Errorf("read %s --section %s: line %d is invalid — lines are 1-indexed", path, section, start)
	}
	if start > total {
		return 0, 0, fmt.Errorf("read %s --section %s: starts past EOF (file has %d lines)", path, section, total)
	}
	if end > total {
		end = total
	}
	return start, end, nil
}

func numericWindow(r io.Reader, start, end int) ([]string, int, error) {
	reader := bufio.NewReader(r)
	var lines []string
	var selected strings.Builder
	total := 0
	pending := false
	for {
		fragment, err := reader.ReadSlice('\n')
		terminated := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if terminated {
			fragment = fragment[:len(fragment)-1]
		}
		pending = pending || len(fragment) > 0
		if total+1 >= start && total+1 <= end && start > 0 {
			selected.Write(fragment)
		}
		if terminated || errors.Is(err, io.EOF) && pending {
			total++
			if total >= start && total <= end && start > 0 {
				lines = append(lines, selected.String())
				selected.Reset()
			}
			pending = false
		}
		if errors.Is(err, io.EOF) {
			return lines, total, nil
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return nil, 0, err
		}
	}
}
