package relocate

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func openRetirementTree(job *cleanup.Job) (*os.Root, error) {
	tree, err := os.OpenRoot(job.Original)
	if err != nil {
		return nil, err
	}
	info, err := tree.Stat(".")
	if err != nil {
		_ = tree.Close()
		return nil, err
	}
	if held := cleanup.IDOf(info); held != job.Tree {
		_ = tree.Close()
		return nil, fmt.Errorf("%s holds tree %s, the job captured tree %s", job.Original, label(held), label(job.Tree))
	}
	return tree, nil
}

func swapRetirementLink(tree *os.Root, from, to string) error {
	file, err := tree.OpenFile(".git", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxLinkage {
		_ = file.Close()
		return errNotLinkage
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxLinkage))
	closeErr := file.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if string(data) != from {
		return fmt.Errorf(".git reads %q, want %q", data, from)
	}
	name := ".git." + rand.Text()
	temp, err := tree.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() { _ = temp.Close() }()
	defer func() { _ = tree.Remove(name) }()
	if _, err := temp.Write([]byte(to)); err != nil {
		return err
	}
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := tree.Rename(name, ".git"); err != nil {
		return err
	}
	dir, err := tree.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
