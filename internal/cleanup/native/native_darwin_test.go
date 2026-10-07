package native

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	backgroundHelperEnv = "CCX_NATIVE_HELPER_BACKGROUND"
	insideHelperEnv     = "CCX_NATIVE_HELPER_INSIDE"
	deepHelperEnv       = "CCX_NATIVE_HELPER_DEEP"
	threadHelperEnv     = "CCX_NATIVE_HELPER_THREAD"
	vnodeDirectory      = 2

	stagedFD   = 7
	repeatedFD = 8
	besideFD   = 9

	asleep = "/bin/sleep 60"
	reader = `/bin/sleep 60 < "$0"`
	argued = `/bin/bash -c "read line" arg0 "$0" <&3`
)

func TestLayouts(t *testing.T) {
	var (
		vnode     vnodeInfo
		fdVnode   vnodeFDInfo
		vnodePath vnodeInfoPath
		fdPath    vnodeFDInfoWithPath
		cwdPaths  procVnodePathInfo
		thread    procThreadPathInfo
		bsd       procBSDInfo
		fd        procFDInfo
		unique    procUniqIdentifierInfo
		coalition procPIDCoalitionInfo
		usage     coalitionResourceUsage
		timebase  machTimebase
	)
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"sizeof vnode_info", unsafe.Sizeof(vnode), 152},
		{"offsetof vnode_info.vi_stat.vst_dev", unsafe.Offsetof(vnode.dev), 0},
		{"offsetof vnode_info.vi_stat.vst_ino", unsafe.Offsetof(vnode.ino), 8},
		{"offsetof vnode_info.vi_type", unsafe.Offsetof(vnode.kind), 136},
		{"offsetof vnode_info.vi_fsid", unsafe.Offsetof(vnode.fsid), 144},
		{"sizeof vnode_fdinfo", unsafe.Sizeof(fdVnode), 176},
		{"offsetof vnode_fdinfo.pvi", unsafe.Offsetof(fdVnode.vnode), 24},
		{"sizeof vnode_info_path", unsafe.Sizeof(vnodePath), 1176},
		{"offsetof vnode_info_path.vip_vi.vi_stat.vst_dev", unsafe.Offsetof(vnodePath.dev), 0},
		{"offsetof vnode_info_path.vip_vi.vi_stat.vst_ino", unsafe.Offsetof(vnodePath.ino), 8},
		{"offsetof vnode_info_path.vip_vi.vi_type", unsafe.Offsetof(vnodePath.kind), 136},
		{"offsetof vnode_info_path.vip_vi.vi_fsid", unsafe.Offsetof(vnodePath.fsid), 144},
		{"offsetof vnode_info_path.vip_path", unsafe.Offsetof(vnodePath.path), 152},
		{"sizeof vnode_fdinfowithpath", unsafe.Sizeof(fdPath), 1200},
		{"offsetof vnode_fdinfowithpath.pvip", unsafe.Offsetof(fdPath.vnode), 24},
		{"sizeof proc_vnodepathinfo", unsafe.Sizeof(cwdPaths), 2352},
		{"offsetof proc_vnodepathinfo.pvi_cdir", unsafe.Offsetof(cwdPaths.cdir), 0},
		{"sizeof proc_threadwithpathinfo", unsafe.Sizeof(thread), 1288},
		{"offsetof proc_threadwithpathinfo.pvip", unsafe.Offsetof(thread.cdir), 112},
		{"sizeof proc_bsdinfo", unsafe.Sizeof(bsd), 136},
		{"offsetof proc_bsdinfo.pbi_flags", unsafe.Offsetof(bsd.flags), 0},
		{"offsetof proc_bsdinfo.pbi_status", unsafe.Offsetof(bsd.status), 4},
		{"offsetof proc_bsdinfo.pbi_ppid", unsafe.Offsetof(bsd.ppid), 16},
		{"offsetof proc_bsdinfo.pbi_uid", unsafe.Offsetof(bsd.uid), 20},
		{"offsetof proc_bsdinfo.pbi_ruid", unsafe.Offsetof(bsd.ruid), 28},
		{"offsetof proc_bsdinfo.pbi_comm", unsafe.Offsetof(bsd.comm), 48},
		{"offsetof proc_bsdinfo.pbi_name", unsafe.Offsetof(bsd.name), 64},
		{"offsetof proc_bsdinfo.e_tdev", unsafe.Offsetof(bsd.tdev), 108},
		{"offsetof proc_bsdinfo.pbi_start_tvsec", unsafe.Offsetof(bsd.start), 120},
		{"offsetof proc_bsdinfo.pbi_start_tvusec", unsafe.Offsetof(bsd.startMicros), 128},
		{"sizeof proc_fdinfo", unsafe.Sizeof(fd), 8},
		{"offsetof proc_fdinfo.proc_fd", unsafe.Offsetof(fd.fd), 0},
		{"offsetof proc_fdinfo.proc_fdtype", unsafe.Offsetof(fd.kind), 4},
		{"sizeof proc_uniqidentifierinfo", unsafe.Sizeof(unique), 56},
		{"offsetof proc_uniqidentifierinfo.p_uniqueid", unsafe.Offsetof(unique.uniqueID), 16},
		{"sizeof proc_pidcoalitioninfo", unsafe.Sizeof(coalition), 40},
		{"offsetof proc_pidcoalitioninfo.coalition_id", unsafe.Offsetof(coalition.ids), 0},
		{"sizeof coalition_resource_usage buffer", unsafe.Sizeof(usage), 1024},
		{"offsetof coalition_resource_usage.cpu_time", unsafe.Offsetof(usage.cpuTime), 24},
		{"sizeof mach_timebase_info", unsafe.Sizeof(timebase), 8},
		{"offsetof mach_timebase_info.denom", unsafe.Offsetof(timebase.denom), 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %d, want %d", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestWithin(t *testing.T) {
	tests := []struct {
		name string
		root string
		path string
		want bool
	}{
		{"the root itself", "/work/tree", "/work/tree", true},
		{"a child", "/work/tree", "/work/tree/sub", true},
		{"a deep descendant", "/work/tree", "/work/tree/a/b/c", true},
		{"a sibling sharing the prefix", "/work/tree", "/work/tree-2", false},
		{"a sibling's child", "/work/tree", "/work/tree-2/sub", false},
		{"the parent", "/work/tree", "/work", false},
		{"no working directory", "/work/tree", "", false},
		{"the filesystem root itself", "/", "/", true},
		{"a descendant of the filesystem root", "/", "/Users/alice/repo", true},
		{"no working directory under the filesystem root", "/", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := within(tt.root, tt.path); got != tt.want {
				t.Errorf("within(%q, %q) = %t, want %t", tt.root, tt.path, got, tt.want)
			}
		})
	}
}

func vnodeRecord(t *testing.T, dir, path string) *vnodeInfoPath {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Lstat(dir, &stat); err != nil {
		t.Fatalf("Lstat(%s): %v", dir, err)
	}
	var volume unix.Statfs_t
	if err := unix.Statfs(dir, &volume); err != nil {
		t.Fatalf("Statfs(%s): %v", dir, err)
	}
	record := &vnodeInfoPath{dev: stat.Dev, ino: stat.Ino, kind: vnodeDirectory, fsid: volume.Fsid.Val}
	copy(record.path[:], path)
	return record
}

func hide(t *testing.T, base string) string {
	t.Helper()
	locked := filepath.Join(base, "locked")
	hidden := filepath.Join(locked, "hidden")
	if err := os.MkdirAll(hidden, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // restores a fixture directory so it can be removed
	return hidden
}

func lock(t *testing.T, base string) {
	t.Helper()
	if err := os.Chmod(filepath.Join(base, "locked"), 0); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
}

func TestLocate(t *testing.T) {
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatalf("loadLibSystem: %v", err)
	}
	base := realPath(t, t.TempDir())
	tree := filepath.Join(base, "tree")
	other := filepath.Join(base, "other")
	removed := filepath.Join(base, "removed")
	for _, dir := range []string{tree, other, removed} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
	}
	unlinked := vnodeRecord(t, removed, removed)
	if err := os.Remove(removed); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	unmounted := vnodeRecord(t, tree, "/tree")
	unmounted.fsid = [2]int32{-1, -1}
	unreachable := vnodeRecord(t, hide(t, base), filepath.Join(base, "locked", "hidden"))

	lock(t, base)

	tests := []struct {
		name       string
		record     *vnodeInfoPath
		wantPath   string
		wantLinked bool
		wantErr    error
	}{
		{"no vnode", &vnodeInfoPath{}, "", false, nil},
		{"a path naming the directory", vnodeRecord(t, tree, tree), tree, true, nil},
		{"a path cut to its last component", vnodeRecord(t, tree, "/tree"), tree, true, nil},
		{"a path naming another directory", vnodeRecord(t, tree, other), tree, true, nil},
		{"no path", vnodeRecord(t, tree, ""), tree, true, nil},
		{"a removed directory", unlinked, "", false, nil},
		{"a cut path on a filesystem that resolves no inodes", unmounted, "", false, unix.ENOTSUP},
		{"a directory below one this process may not search", unreachable, "", false, unix.EACCES},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, linked, err := lib.locate(tt.record)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("locate = %v, want %v", err, tt.wantErr)
			}
			if path != tt.wantPath || linked != tt.wantLinked {
				t.Errorf("locate = %q, linked %t; want %q, linked %t", path, linked, tt.wantPath, tt.wantLinked)
			}
		})
	}
}

func procargs(count int32, program string, args, env []string) []byte {
	raw := binary.NativeEndian.AppendUint32(nil, uint32(count)) //nolint:gosec // argc is a C int
	raw = append(raw, program...)
	raw = append(raw, make([]byte, argumentAlignment-len(program)%argumentAlignment)...)
	for _, word := range slices.Concat(args, env) {
		raw = append(raw, word...)
		raw = append(raw, 0)
	}
	return raw
}

func TestArguments(t *testing.T) {
	tests := []struct {
		name    string
		raw     []byte
		want    []string
		wantErr bool
	}{
		{"the arguments and never the environment", procargs(2, "/bin/cat", []string{"cat", "/tree/file"}, []string{"PWD=/tree"}), []string{"cat", "/tree/file"}, false},
		{"an empty argument", procargs(3, "/bin/sh", []string{"sh", "", "x"}, []string{"HOME=/tree"}), []string{"sh", "", "x"}, false},
		{"an empty first argument", procargs(2, "/bin/sleep", []string{"", "2"}, []string{"REVIEW_ONLY_PATH=/tree"}), []string{"", "2"}, false},
		{"only empty arguments", procargs(2, "/bin/sh", []string{"", ""}, []string{"HOME=/tree"}), []string{"", ""}, false},
		{"a program path ending on the alignment", procargs(1, "/bin/sh", []string{"/tree"}, []string{"HOME=/tree"}), []string{"/tree"}, false},
		{"a program path one byte past the alignment", procargs(1, "/bin/cat", []string{"/tree"}, []string{"HOME=/tree"}), []string{"/tree"}, false},
		{"no arguments", procargs(0, "/bin/sh", nil, []string{"HOME=/tree"}), nil, false},
		{"a program path without its terminator", append(binary.NativeEndian.AppendUint32(nil, 1), "/bin/sh"...), nil, true},
		{"no room for a count", []byte{2, 0}, nil, true},
		{"fewer arguments than counted", procargs(3, "/bin/sh", []string{"sh", "-c"}, nil), nil, true},
		{"a last argument cut short", append(procargs(2, "/bin/sh", []string{"sh"}, nil), "/tree"...), nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := arguments(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("arguments = %v, want an error: %t", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("arguments = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNames(t *testing.T) {
	s := &scan{root: "/private/var/x/tree", given: "/var/x/tree"}
	tests := []struct {
		name string
		arg  string
		want string
	}{
		{"a path below the kernel's spelling", "/private/var/x/tree/a", "/private/var/x/tree/a"},
		{"a path below the caller's spelling", "/var/x/tree/a", "/var/x/tree/a"},
		{"the tree itself", "/var/x/tree", "/var/x/tree"},
		{"a flag's value", "--work-tree=/private/var/x/tree/a", "/private/var/x/tree/a"},
		{"the value after the last equals sign", "a=b=/var/x/tree/c", "/var/x/tree/c"},
		{"an uncleaned path", "/private/var/x/tree/./a/../b/", "/private/var/x/tree/b"},
		{"a path that climbs out of the tree", "/private/var/x/tree/../other", ""},
		{"a relative path", "tree/a", ""},
		{"a relative flag value", "--work-tree=tree/a", ""},
		{"a command string mentioning the tree", "cd /private/var/x/tree && ls", ""},
		{"a sibling sharing the prefix", "/private/var/x/tree-2/a", ""},
		{"the parent", "/private/var/x", ""},
		{"an empty argument", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, named := s.names(tt.arg)
			if got != tt.want || named != (tt.want != "") {
				t.Errorf("names(%q) = %q, %t; want %q, %t", tt.arg, got, named, tt.want, tt.want != "")
			}
		})
	}
}

func TestArgumentNeverReadsTheEnvironment(t *testing.T) {
	f := plant(t)
	unnamed := &exec.Cmd{
		Path:        "/bin/sleep",
		Args:        []string{"", "60"},
		Env:         []string{"REVIEW_ONLY_PATH=" + f.real + "/sub"},
		Dir:         f.outside,
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}
	if err := unnamed.Start(); err != nil {
		t.Fatalf("start sleep with an empty first argument: %v", err)
	}
	t.Cleanup(func() { stopSession(unnamed) })

	raw, err := unix.SysctlRaw("kern.procargs2", unnamed.Process.Pid)
	if err != nil {
		t.Fatalf("read the arguments of pid %d: %v", unnamed.Process.Pid, err)
	}
	args, err := arguments(raw)
	if want := []string{"", "60"}; err != nil || !slices.Equal(args, want) {
		t.Errorf("arguments = %q, %v; want %q", args, err, want)
	}
	s := &scan{root: f.real, given: f.tree}
	if path, err := s.argument(unnamed.Process.Pid, false); path != "" || err != nil {
		t.Errorf("argument = %q, %v; want no evidence from the environment", path, err)
	}
}

func at[T any](address uintptr) *T {
	return *(**T)(unsafe.Pointer(&address)) //nolint:gosec // the address of a buffer the caller pinned
}

func served(t *testing.T, reply func(first, second, third uintptr) (result uintptr, errno unix.Errno)) uintptr {
	t.Helper()
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatalf("loadLibSystem: %v", err)
	}
	return purego.NewCallback(func(_, first, second, third uintptr) uintptr {
		result, errno := reply(first, second, third)
		if errno != 0 {
			*lib.errno() = int32(errno) //nolint:gosec // errno is a C int
		}
		return result
	})
}

func answering(t *testing.T, reply func(selector int, buf uintptr) (filled uintptr, errno unix.Errno)) uintptr {
	t.Helper()
	return served(t, func(selector, _, buf uintptr) (uintptr, unix.Errno) {
		return reply(int(selector), buf) //nolint:gosec // a flavor or a descriptor number
	})
}

func sequenced[T any](values []T, read int) T {
	if len(values) == 0 {
		var none T
		return none
	}
	return values[min(read, len(values)-1)]
}

type staged struct {
	parent     uint32
	identities []unix.Errno
	starts     []uint64
	micros     []uint64
	reads      int
	effective  uint32
	actual     uint32
	terminal   uint32
	cwd        *vnodeInfoPath
	open       *vnodeInfoPath
	denied     unix.Errno
	vnode      *vnodeInfoPath
	opaque     unix.Errno
	repeated   bool
	beside     *vnodeInfoPath
	unlisted   unix.Errno
	programs   []string
	located    int
	unlocated  unix.Errno
	mount      string
	volume     uint32
	unmounted  unix.Errno
	signing    uint32
	unsigned   unix.Errno
}

func agent(program string, started uint64) staged {
	uid := uint32(os.Getuid()) //nolint:gosec // a uid is a C unsigned int
	return staged{
		parent: launchd, starts: []uint64{started}, effective: uid, actual: uid,
		programs: []string{program}, mount: "/", volume: 0x4480d001, signing: 0x26017b01,
		denied: unix.EPERM, opaque: unix.EPERM,
	}
}

func (k *staged) listed() []procFDInfo {
	var fds []procFDInfo
	if k.open != nil || k.denied != 0 {
		fds = append(fds, procFDInfo{fd: stagedFD, kind: fdTypeVnode})
	}
	if k.repeated {
		fds = append(fds, procFDInfo{fd: repeatedFD, kind: fdTypeVnode})
	}
	if k.beside != nil {
		fds = append(fds, procFDInfo{fd: besideFD, kind: fdTypeVnode})
	}
	return fds
}

func (k *staged) lib(t *testing.T) *libSystem {
	t.Helper()
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatalf("loadLibSystem: %v", err)
	}
	info := answering(t, func(flavor int, buf uintptr) (uintptr, unix.Errno) {
		switch flavor {
		case flavorBSDInfo:
			if errno := sequenced(k.identities, k.reads); errno != 0 {
				k.reads++
				return 0, errno
			}
			bsd := at[procBSDInfo](buf)
			*bsd = procBSDInfo{
				status: 2, ppid: k.parent, uid: k.effective, ruid: k.actual, tdev: noDevice,
				start: sequenced(k.starts, k.reads), startMicros: sequenced(k.micros, k.reads),
			}
			if k.terminal != 0 {
				bsd.tdev = k.terminal
			}
			copy(bsd.name[:], "staged")
			k.reads++
			return unsafe.Sizeof(*bsd), 0
		case flavorVnodePathInfo:
			paths := at[procVnodePathInfo](buf)
			if k.cwd != nil {
				paths.cdir = *k.cwd
			}
			return unsafe.Sizeof(*paths), 0
		case flavorListFDs:
			listed := k.listed()
			if buf == 0 {
				return uintptr(len(listed)+1) * unsafe.Sizeof(procFDInfo{}), 0 //nolint:gosec // a count of staged descriptors
			}
			if k.unlisted != 0 {
				return 0, k.unlisted
			}
			copy(unsafe.Slice(at[procFDInfo](buf), len(listed)), listed) //nolint:gosec // a buffer the caller pinned with room for the len(listed)+1 descriptors reported above
			return uintptr(len(listed)) * unsafe.Sizeof(procFDInfo{}), 0 //nolint:gosec // a count of staged descriptors
		}
		return 0, unix.EINVAL
	})
	descriptor := served(t, func(fd, flavor, buf uintptr) (uintptr, unix.Errno) {
		switch {
		case flavor == fdFlavorVnodePathInfo && fd == besideFD:
			described := at[vnodeFDInfoWithPath](buf)
			described.vnode = *k.beside
			return unsafe.Sizeof(*described), 0
		case flavor == fdFlavorVnodePathInfo && k.denied != 0:
			return 0, k.denied
		case flavor == fdFlavorVnodePathInfo:
			described := at[vnodeFDInfoWithPath](buf)
			described.vnode = *k.open
			return unsafe.Sizeof(*described), 0
		case flavor == fdFlavorVnodeInfo && k.opaque != 0:
			return 0, k.opaque
		case flavor == fdFlavorVnodeInfo && k.vnode != nil:
			identified := at[vnodeFDInfo](buf)
			identified.vnode = vnodeInfo{dev: k.vnode.dev, ino: k.vnode.ino, kind: k.vnode.kind, fsid: k.vnode.fsid}
			return unsafe.Sizeof(*identified), 0
		}
		return 0, unix.EINVAL
	})
	program := served(t, func(buf, _, _ uintptr) (uintptr, unix.Errno) {
		path := sequenced(k.programs, k.located)
		k.located++
		if k.unlocated != 0 {
			return 0, k.unlocated
		}
		if path == "" {
			return 0, unix.ESRCH
		}
		copy(unsafe.Slice(at[byte](buf), pidPathBytes), path) //nolint:gosec // a buffer the caller pinned at pidPathBytes
		return uintptr(len(path)), 0                          //nolint:gosec // the length of a staged path
	})
	signing := served(t, func(_, buf, _ uintptr) (uintptr, unix.Errno) {
		if k.unsigned != 0 {
			return ^uintptr(0), k.unsigned
		}
		*at[uint32](buf) = k.signing
		return 0, 0
	})
	statfs := func(path string, volume *unix.Statfs_t) error {
		if k.unmounted != 0 {
			return k.unmounted
		}
		if !slices.Contains(k.programs, path) {
			return unix.ENOENT
		}
		copy(volume.Mntonname[:], k.mount)
		volume.Flags = k.volume
		return nil
	}
	return &libSystem{
		procPIDInfo: info, procPIDFDInfo: descriptor, procPIDPath: program, csOps: signing,
		fsGetPath: lib.fsGetPath, errno: lib.errno, statfs: statfs,
	}
}

func TestDescriptor(t *testing.T) {
	f := plant(t)
	inside := vnodeRecord(t, f.sub, f.real+"/sub")
	beside := vnodeRecord(t, f.outside, realPath(t, f.outside))
	misspelled := vnodeRecord(t, hide(t, f.tree), filepath.Join(hide(t, f.outside), "file"))
	lock(t, f.tree)
	lock(t, f.outside)

	tests := []struct {
		name    string
		open    *vnodeInfoPath
		denied  unix.Errno
		want    string
		wantErr error
	}{
		{"a directory in the tree", inside, 0, f.real + "/sub", nil},
		{"a directory beside the tree", beside, 0, "", nil},
		{"a descriptor closed since it was listed", nil, unix.EBADF, "", nil},
		{"a descriptor whose file is gone", nil, unix.ENOENT, "", nil},
		{"a descriptor the kernel refuses to describe", nil, unix.EPERM, "", unix.EPERM},
		{"a descriptor the kernel fails to read", nil, unix.EIO, "", unix.EIO},
		{"a directory in the tree spelled beside it, neither spelling provable", misspelled, 0, "", unix.EACCES},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kernel := &staged{open: tt.open, denied: tt.denied}
			s := &scan{lib: kernel.lib(t), root: f.real, given: f.tree}
			path, err := s.descriptor(cleanup.ProcessID{PID: 4242})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("descriptor = %q, %v; want %v", path, err, tt.wantErr)
			}
			if path != tt.want {
				t.Errorf("descriptor = %q, want %q", path, tt.want)
			}
		})
	}
}

func TestDescriptorsThatCannotBeListed(t *testing.T) {
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatalf("loadLibSystem: %v", err)
	}
	refused := &libSystem{errno: lib.errno, procPIDInfo: answering(t, func(_ int, buf uintptr) (uintptr, unix.Errno) {
		if buf == 0 {
			return 2 * unsafe.Sizeof(procFDInfo{}), 0
		}
		return 0, unix.EPERM
	})}
	if fds, err := refused.listFDs(4242); !errors.Is(err, unix.EPERM) || fds != nil {
		t.Errorf("listFDs = %+v, %v; want none and EPERM", fds, err)
	}
	s := &scan{lib: refused, root: "/tree", given: "/tree"}
	if path, err := s.descriptor(cleanup.ProcessID{PID: 4242}); !errors.Is(err, unix.EPERM) || path != "" {
		t.Errorf("descriptor = %q, %v; want no path and EPERM", path, err)
	}
}

func TestInspectSelectsEvidence(t *testing.T) {
	const started = 100
	f := plant(t)
	arguer := spawn(t, f.outside, unfed(t), "/bin/bash", "-c", "read line", "arg0", f.real+"/sub").Process.Pid
	sleeper := spawn(t, f.outside, nil, "/bin/sleep", "60").Process.Pid
	works := vnodeRecord(t, f.sub, f.real+"/sub")
	opens := vnodeRecord(t, f.file, f.real+"/file")
	exactly := func(pid int, start int64) []cleanup.ProcessID {
		return []cleanup.ProcessID{{PID: pid, Start: start}}
	}

	tests := []struct {
		name    string
		pid     int
		kernel  staged
		scan    scan
		want    *cleanup.Holder
		wantErr bool
	}{
		{"a working directory before anything else", arguer, staged{cwd: works, open: opens}, scan{}, &cleanup.Holder{Evidence: cleanup.EvidenceCwd, Path: f.real + "/sub"}, false},
		{"a descriptor before an argument", arguer, staged{open: opens}, scan{}, &cleanup.Holder{Evidence: cleanup.EvidenceFD, Path: f.real + "/file"}, false},
		{"an argument last", arguer, staged{}, scan{}, &cleanup.Holder{Evidence: cleanup.EvidenceArgv, Path: f.real + "/sub"}, false},
		{"nothing in the tree", sleeper, staged{}, scan{}, nil, false},
		{"a pid gone once the kernel refused to identify it", arguer, staged{identities: []unix.Errno{unix.EPERM, unix.ESRCH}, cwd: works}, scan{}, nil, false},
		{"a live pid the kernel refuses to identify", arguer, staged{identities: []unix.Errno{unix.EPERM}, cwd: works}, scan{}, nil, true},
		{"the requester's arguments", arguer, staged{}, scan{requester: cleanup.ProcessID{PID: arguer, Start: started}, named: true}, nil, false},
		{"not the arguments of a pid no requester named", arguer, staged{}, scan{requester: cleanup.ProcessID{PID: arguer, Start: started}}, &cleanup.Holder{Evidence: cleanup.EvidenceArgv, Path: f.real + "/sub"}, false},
		{"not another requester's arguments", arguer, staged{}, scan{requester: cleanup.ProcessID{PID: sleeper, Start: started}, named: true}, &cleanup.Holder{Evidence: cleanup.EvidenceArgv, Path: f.real + "/sub"}, false},
		{"not the arguments of a requester started a second later", arguer, staged{}, scan{requester: cleanup.ProcessID{PID: arguer, Start: started + 1}, named: true}, &cleanup.Holder{Evidence: cleanup.EvidenceArgv, Path: f.real + "/sub"}, false},
		{"not the requester's descriptor", arguer, staged{open: opens}, scan{requester: cleanup.ProcessID{PID: arguer, Start: started}, named: true}, &cleanup.Holder{Evidence: cleanup.EvidenceFD, Path: f.real + "/file"}, false},
		{"not the requester's working directory", arguer, staged{cwd: works}, scan{requester: cleanup.ProcessID{PID: arguer, Start: started}, named: true}, &cleanup.Holder{Evidence: cleanup.EvidenceCwd, Path: f.real + "/sub"}, false},
		{"a retiring watcher's descriptor", sleeper, staged{open: opens}, scan{retiring: exactly(sleeper, started)}, nil, false},
		{"a retiring watcher's descriptor the kernel refuses to describe", sleeper, staged{denied: unix.EPERM}, scan{retiring: exactly(sleeper, started)}, nil, false},
		{"not a retiring watcher's arguments", arguer, staged{open: opens}, scan{retiring: exactly(arguer, started)}, &cleanup.Holder{Evidence: cleanup.EvidenceArgv, Path: f.real + "/sub"}, false},
		{"not a retiring watcher's working directory", sleeper, staged{cwd: works, open: opens}, scan{retiring: exactly(sleeper, started)}, &cleanup.Holder{Evidence: cleanup.EvidenceCwd, Path: f.real + "/sub"}, false},
		{"not a watcher started a second later", sleeper, staged{open: opens}, scan{retiring: exactly(sleeper, started+1)}, &cleanup.Holder{Evidence: cleanup.EvidenceFD, Path: f.real + "/file"}, false},
		{"not a watcher started a second earlier", sleeper, staged{open: opens}, scan{retiring: exactly(sleeper, started-1)}, &cleanup.Holder{Evidence: cleanup.EvidenceFD, Path: f.real + "/file"}, false},
		{"not another pid started with the watcher", sleeper, staged{open: opens}, scan{retiring: exactly(arguer, started)}, &cleanup.Holder{Evidence: cleanup.EvidenceFD, Path: f.real + "/file"}, false},
		{"not a pid that passed to another process once the watcher was identified", sleeper, staged{open: opens, starts: []uint64{started, started + 1}}, scan{retiring: exactly(sleeper, started)}, nil, true},
		{"a process whose descriptor the kernel refuses to describe", sleeper, staged{denied: unix.EPERM}, scan{}, nil, true},
		{"the scanning process's own descriptor and arguments", arguer, staged{open: opens}, scan{self: arguer}, nil, false},
		{"not the scanning process's own working directory", arguer, staged{cwd: works}, scan{self: arguer}, &cleanup.Holder{Evidence: cleanup.EvidenceCwd, Path: f.real + "/sub"}, false},
		{"a direct child of the scanning process", arguer, staged{parent: 4242, cwd: works, open: opens}, scan{self: 4242}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kernel := tt.kernel
			if kernel.starts == nil {
				kernel.starts = []uint64{started}
			}
			if kernel.parent == 0 {
				kernel.parent = 1
			}
			s := tt.scan
			s.lib, s.root, s.given = kernel.lib(t), f.real, f.tree
			got, held, err := s.inspect(tt.pid)
			if (err != nil) != tt.wantErr {
				t.Fatalf("inspect = %+v, %t, %v; want an error: %t", got, held, err, tt.wantErr)
			}
			var want cleanup.Holder
			if tt.want != nil {
				want = *tt.want
				want.PID, want.Name = tt.pid, "staged"
			}
			if got != want || held != (tt.want != nil) {
				t.Errorf("inspect = %+v, %t; want %+v, %t", got, held, want, tt.want != nil)
			}
		})
	}
}

func TestApprovedServices(t *testing.T) {
	const started = 100
	want := []string{
		"/System/Library/Frameworks/QuickLookThumbnailing.framework/Support/com.apple.quicklook.ThumbnailsAgent",
		"/System/Library/Frameworks/ClassKit.framework/Versions/A/progressd",
		"/System/Library/PrivateFrameworks/ScreenTimeCore.framework/Versions/A/ScreenTimeAgent",
		"/usr/libexec/dmd",
		"/usr/libexec/routined",
		"/usr/libexec/nsurlsessiond",
		"/usr/libexec/UserEventAgent",
		"/usr/libexec/lsd",
		"/usr/libexec/knowledge-agent",
		"/System/Library/PrivateFrameworks/CoreDuetContext.framework/Versions/A/Resources/ContextStoreAgent",
	}
	if len(approvedServices) != 10 || !slices.Equal(approvedServices, want) {
		t.Fatalf("approvedServices = %q, want exactly %q", approvedServices, want)
	}
	for _, program := range approvedServices {
		t.Run(program, func(t *testing.T) {
			kernel := agent(program, started)
			kernel.micros = []uint64{250}
			s := scan{lib: kernel.lib(t), root: "/tree", given: "/tree"}
			born, err := s.service(cleanup.ProcessID{PID: 4242, Start: started})
			if want := (birth{seconds: started, micros: 250}); err != nil || born != want {
				t.Errorf("service = %+v, %v; want %+v", born, err, want)
			}
			if path, err := s.descriptor(cleanup.ProcessID{PID: 4242, Start: started}); path != "" || err != nil {
				t.Errorf("descriptor = %q, %v; want its refused descriptor skipped", path, err)
			}
		})
	}
}

func TestInspectApprovedServices(t *testing.T) {
	const (
		started      = 100
		lsd          = "/usr/libexec/lsd"
		relocated    = "/private/tmp/x/usr/libexec/lsd"
		contextStore = "/System/Library/PrivateFrameworks/CoreDuetContext.framework/Versions/A/Resources/ContextStoreAgent"
		knowledge    = "/usr/libexec/knowledge-agent"
		trustd       = "/usr/libexec/trustd"
		dataVolume   = "/System/Volumes/Data"
	)
	f := plant(t)
	arguer := spawn(t, f.outside, unfed(t), "/bin/bash", "-c", "read line", "arg0", f.real+"/sub").Process.Pid
	sleeper := spawn(t, f.outside, nil, "/bin/sleep", "60").Process.Pid
	works := vnodeRecord(t, f.sub, f.real+"/sub")
	opens := vnodeRecord(t, f.file, f.real+"/file")
	outside := vnodeRecord(t, f.outside, realPath(t, f.outside))
	unresolved := vnodeRecord(t, f.file, "")
	unresolved.fsid = [2]int32{-1, -1}
	unprovable := vnodeRecord(t, hide(t, f.tree), filepath.Join(hide(t, f.outside), "file"))
	lock(t, f.tree)
	lock(t, f.outside)
	file := &cleanup.Holder{Evidence: cleanup.EvidenceFD, Path: f.real + "/file"}
	approved := func(*staged) {}
	run := func(program string) func(*staged) {
		return func(k *staged) { k.programs = []string{program} }
	}

	tests := []struct {
		name    string
		pid     int
		stage   func(k *staged)
		want    *cleanup.Holder
		wantErr error
		says    string
		checks  int
	}{
		{"a descriptor refused twice with EPERM", sleeper, approved, nil, nil, "", 2},
		{"a descriptor refused twice with EACCES", sleeper, func(k *staged) { k.denied, k.opaque = unix.EACCES, unix.EACCES }, nil, nil, "", 2},
		{"a descriptor refused with EPERM, then EACCES", sleeper, func(k *staged) { k.opaque = unix.EACCES }, nil, nil, "", 2},
		{"a descriptor refused with EACCES, then EPERM", sleeper, func(k *staged) { k.denied = unix.EACCES }, nil, nil, "", 2},
		{"two refused descriptors, their owner verified once before and once after", sleeper, func(k *staged) { k.repeated = true }, nil, nil, "", 2},
		{"a refused descriptor closed before its vnode is read", sleeper, func(k *staged) { k.opaque = unix.EBADF }, nil, nil, "", 2},
		{"a refused descriptor whose file is gone before its vnode is read", sleeper, func(k *staged) { k.opaque = unix.ENOENT }, nil, nil, "", 2},
		{"a refused descriptor whose vnode is beside the tree", sleeper, func(k *staged) { k.opaque, k.vnode = 0, outside }, nil, nil, "", 2},
		{"a refused descriptor whose vnode was removed", sleeper, func(k *staged) { k.opaque, k.vnode = 0, &vnodeInfoPath{} }, nil, nil, "", 2},
		{"a refused descriptor whose vnode is in the tree", sleeper, func(k *staged) { k.opaque, k.vnode = 0, opens }, file, nil, "", 1},
		{"a refused descriptor whose vnode resolves to no path", sleeper, func(k *staged) { k.opaque, k.vnode = 0, unresolved }, nil, unix.ENOTSUP, "", 1},
		{"a refused descriptor whose vnode the kernel fails to read", sleeper, func(k *staged) { k.opaque = unix.EIO }, nil, unix.EIO, "", 1},
		{"a descriptor at a path not proven to name its file, whose inode the kernel refuses to resolve", sleeper, func(k *staged) { k.denied, k.open = 0, unprovable }, nil, nil, "", 2},
		{"such a descriptor whose vnode the kernel describes, then refuses to resolve again", sleeper, func(k *staged) { k.denied, k.open, k.opaque, k.vnode = 0, unprovable, 0, unprovable }, nil, nil, "", 2},
		{"such a descriptor of an unapproved owner", sleeper, func(k *staged) { k.denied, k.open, k.programs = 0, unprovable, []string{trustd} }, nil, unix.EACCES, "does not name: permission denied; its owner is not an approved Apple service: its executable is " + trustd, 1},
		{"a descriptor the kernel fails to read", sleeper, func(k *staged) { k.denied = unix.EIO }, nil, unix.EIO, "", 0},
		{"descriptors that cannot be listed", sleeper, func(k *staged) { k.unlisted = unix.EIO }, nil, unix.EIO, "", 0},
		{"a working directory in the tree", sleeper, func(k *staged) { k.cwd = works }, &cleanup.Holder{Evidence: cleanup.EvidenceCwd, Path: f.real + "/sub"}, nil, "", 0},
		{"an argument naming the tree", arguer, approved, &cleanup.Holder{Evidence: cleanup.EvidenceArgv, Path: f.real + "/sub"}, nil, "", 2},
		{"a readable descriptor in the tree after a refused one", sleeper, func(k *staged) { k.beside = opens }, file, nil, "", 1},
		{"a readable descriptor beside the tree after a refused one", sleeper, func(k *staged) { k.beside = outside }, nil, nil, "", 2},
		{"no refused descriptor, so no owner to verify", sleeper, func(k *staged) { k.denied, k.open, k.programs = 0, outside, []string{relocated} }, nil, nil, "", 0},
		{"ContextStoreAgent", sleeper, run(contextStore), nil, nil, "", 2},
		{"knowledge-agent", sleeper, run(knowledge), nil, nil, "", 2},
		{"trustd", sleeper, run(trustd), nil, unix.EPERM, trustd, 1},
		{"an approved name at another location", sleeper, run(relocated), nil, unix.EPERM, relocated, 1},
		{"an approved path with a suffix", sleeper, run(lsd + "/"), nil, unix.EPERM, lsd + "/", 1},
		{"an unapproved owner refused with EACCES", sleeper, func(k *staged) { k.denied, k.programs = unix.EACCES, []string{trustd} }, nil, unix.EACCES, trustd, 1},
		{"an executable path the kernel withholds", sleeper, func(k *staged) { k.unlocated = unix.ESRCH }, nil, unix.EPERM, "executable path", 1},
		{"an executable on a volume mounted elsewhere", sleeper, func(k *staged) { k.mount = dataVolume }, nil, unix.EPERM, dataVolume, 1},
		{"an executable on the writable data volume", sleeper, func(k *staged) { k.mount, k.volume = dataVolume, 0x04909080 }, nil, unix.EPERM, dataVolume, 1},
		{"an executable on a writable volume", sleeper, func(k *staged) { k.volume &^= unix.MNT_RDONLY }, nil, unix.EPERM, "0x4480d000", 1},
		{"an executable on a volume that is not the root filesystem", sleeper, func(k *staged) { k.volume &^= unix.MNT_ROOTFS }, nil, unix.EPERM, "0x44809001", 1},
		{"an executable on a volume that is no snapshot", sleeper, func(k *staged) { k.volume &^= unix.MNT_SNAPSHOT }, nil, unix.EPERM, "0x480d001", 1},
		{"an executable whose volume cannot be read", sleeper, func(k *staged) { k.unmounted = unix.EIO }, nil, unix.EPERM, "volume", 1},
		{"a code signing status the kernel withholds", sleeper, func(k *staged) { k.unsigned = unix.EINVAL }, nil, unix.EPERM, "code signing status", 1},
		{"a binary that is not the platform's", sleeper, func(k *staged) { k.signing &^= csPlatformBinary }, nil, unix.EPERM, "0x22017b01", 1},
		{"a signature that is not valid", sleeper, func(k *staged) { k.signing &^= csValid }, nil, unix.EPERM, "0x26017b00", 1},
		{"a process being debugged", sleeper, func(k *staged) { k.signing |= csDebugged }, nil, unix.EPERM, "0x36017b01", 1},
		{"a parent other than launchd", sleeper, func(k *staged) { k.parent = 4242 }, nil, unix.EPERM, "pid 4242", 1},
		{"a controlling terminal", sleeper, func(k *staged) { k.terminal = 0x10000005 }, nil, unix.EPERM, "0x10000005", 1},
		{"another effective uid", sleeper, func(k *staged) { k.effective++ }, nil, unix.EPERM, "uid", 1},
		{"another real uid", sleeper, func(k *staged) { k.actual++ }, nil, unix.EPERM, "real uid", 1},
		{"a pid that passed to another process before its owner was verified", sleeper, func(k *staged) { k.starts = []uint64{started, started + 1} }, nil, unix.EPERM, "started at 101", 1},
		{"a pid that passed to a process started a second later while its descriptors were read", sleeper, func(k *staged) { k.starts = []uint64{started, started, started + 1} }, nil, unix.EPERM, "started at 101", 2},
		{"a pid that passed to a process started a microsecond later while its descriptors were read", sleeper, func(k *staged) { k.micros = []uint64{5, 5, 6} }, nil, unix.EPERM, "100.000006", 2},
		{"a process that stopped being the service while its descriptors were read", sleeper, func(k *staged) { k.programs = []string{lsd, relocated} }, nil, unix.EPERM, relocated, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kernel := agent(lsd, started)
			tt.stage(&kernel)
			s := scan{lib: kernel.lib(t), root: f.real, given: f.tree}
			got, held, err := s.inspect(tt.pid)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("inspect = %+v, %t, %v; want %v", got, held, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tt.says) {
				t.Errorf("inspect = %v, want it to say %q", err, tt.says)
			}
			var want cleanup.Holder
			if tt.want != nil {
				want = *tt.want
				want.PID, want.Name = tt.pid, "staged"
			}
			if got != want || held != (tt.want != nil) {
				t.Errorf("inspect = %+v, %t; want %+v, %t", got, held, want, tt.want != nil)
			}
			if kernel.located != tt.checks {
				t.Errorf("inspect verified the owner %d times, want %d", kernel.located, tt.checks)
			}
		})
	}
}

func TestKernelDescribesItsOwnProcess(t *testing.T) {
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatalf("loadLibSystem: %v", err)
	}
	f := plant(t)
	file, err := os.Open(f.file)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	want := vnodeRecord(t, f.file, "")

	node, err := lib.vnodeIdentityOfFD(os.Getpid(), int32(file.Fd())) //nolint:gosec // a descriptor fits int32
	if err != nil {
		t.Fatalf("vnodeIdentityOfFD: %v", err)
	}
	if node.dev != want.dev || node.ino != want.ino || node.fsid != want.fsid || node.kind == vnodeNone {
		t.Errorf("vnodeIdentityOfFD = dev %d, inode %d, filesystem %v, type %d; want dev %d, inode %d, filesystem %v, and a type", node.dev, node.ino, node.fsid, node.kind, want.dev, want.ino, want.fsid)
	}
	s := &scan{lib: lib, root: f.real, given: f.tree}
	if path, err := s.heldByID(node); path != f.real+"/file" || err != nil {
		t.Errorf("heldByID = %q, %v; want %q", path, err, f.real+"/file")
	}

	status, err := lib.codeSigningStatus(os.Getpid())
	if err != nil {
		t.Fatalf("codeSigningStatus: %v", err)
	}
	if status&csPlatformBinary != 0 {
		t.Errorf("codeSigningStatus = %#x, which marks this test a platform binary", status)
	}
	if status, err := lib.codeSigningStatus(exitedPID(t)); !errors.Is(err, unix.ESRCH) {
		t.Errorf("codeSigningStatus of an exited process = %#x, %v; want ESRCH", status, err)
	}
}

func exitedPID(t *testing.T) int {
	t.Helper()
	child := exec.Command("/usr/bin/true")
	if err := child.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return child.Process.Pid
}

func spawn(t *testing.T, dir string, stdin *os.File, argv ...string) *exec.Cmd {
	t.Helper()
	child := exec.Command(argv[0], argv[1:]...) //nolint:gosec // fixed system binaries
	child.Dir = dir
	if stdin != nil {
		child.Stdin = stdin
	}
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		t.Fatalf("start %q in %s: %v", argv, dir, err)
	}
	t.Cleanup(func() { stopSession(child) })
	return child
}

func stopSession(leader *exec.Cmd) {
	_ = syscall.Kill(-leader.Process.Pid, syscall.SIGKILL)
	_ = leader.Wait()
}

func unfed(t *testing.T) *os.File {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = write.Close()
		_ = read.Close()
	})
	return read
}

func awaitProgram(t *testing.T, pid int, program string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := unix.SysctlRaw("kern.procargs2", pid)
		if err == nil {
			if args, err := arguments(raw); err == nil && len(args) > 0 && args[0] == program {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never ran %s: %v", pid, program, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func awaitGone(pid int) {
	for deadline := time.Now().Add(10 * time.Second); unix.Kill(pid, 0) == nil && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
}

func grandchild(t *testing.T, dir, script string, args ...string) (pid int, stop func()) {
	t.Helper()
	shell := exec.Command("/bin/sh", append([]string{"-c", script + " & echo $!; wait"}, args...)...) //nolint:gosec // fixed scripts over fixture paths
	shell.Dir = dir
	shell.ExtraFiles = []*os.File{unfed(t)}
	shell.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := shell.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := shell.Start(); err != nil {
		t.Fatalf("start %q in %s: %v", script, dir, err)
	}
	stop = sync.OnceFunc(func() {
		stopSession(shell)
		if pid != 0 {
			awaitGone(pid)
		}
	})
	t.Cleanup(stop)
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("read the pid %q started: %v", script, err)
	}
	pid, err = strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("parse the pid %q started: %v", script, err)
	}
	awaitProgram(t, pid, strings.Fields(script)[0])
	return pid, stop
}

func startOf(t *testing.T, pid int) int64 {
	t.Helper()
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		t.Fatalf("read the start time of pid %d: %v", pid, err)
	}
	return process.Proc.P_starttime.Sec
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	return resolved
}

func descends(pid int) bool {
	for pid > 1 {
		if pid == os.Getpid() {
			return true
		}
		process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			return false
		}
		pid = int(process.Eproc.Ppid)
	}
	return false
}

func surveyed(ctx context.Context, t *testing.T, tree string, refusal error) (verdict error, standing bool) {
	t.Helper()
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatalf("loadLibSystem: %v", err)
	}
	s, err := newScan(ctx, lib, tree)
	if err != nil {
		t.Fatalf("newScan(%s): %v", tree, err)
	}
	pids, err := lib.listPIDs(procUIDOnly, os.Getuid())
	if err != nil {
		t.Fatalf("listPIDs: %v", err)
	}
	var held []cleanup.Holder
	for _, pid := range pids {
		holder, holds, err := s.inspect(int(pid))
		switch {
		case err == nil && holds:
			held = append(held, holder)
		case err == nil:
		case descends(int(pid)):
			t.Fatalf("Guard(%s) cannot inspect pid %d, which this test started: %v", tree, pid, err)
		default:
			standing = standing || strings.HasPrefix(refusal.Error(), fmt.Sprintf("native: inspect pid %d ", pid))
		}
	}
	return active(tree, held), standing
}

func holders(ctx context.Context, t *testing.T, tree string) []cleanup.Holder {
	t.Helper()
	var verdict *cleanup.ActiveError
	err := Guard(ctx, tree)
	for attempt := 0; attempt < 100 && err != nil && !errors.As(err, &verdict); attempt++ {
		if !errors.Is(err, unix.EIO) && !errors.Is(err, unix.EINVAL) {
			if survey, standing := surveyed(ctx, t, tree, err); standing {
				t.Logf("this host keeps Guard from a verdict, so every process it can inspect was inspected instead: %v", err)
				err = survey
				break
			}
		}
		t.Logf("an unrelated process was unreadable in passing: %v", err)
		time.Sleep(20 * time.Millisecond)
		err = Guard(ctx, tree)
	}
	if err == nil {
		return nil
	}
	if !errors.As(err, &verdict) {
		t.Fatalf("Guard(%s) = %v, want a verdict", tree, err)
	}
	if verdict.Worktree != tree {
		t.Errorf("Worktree = %q, want %q", verdict.Worktree, tree)
	}
	return verdict.Holders
}

func byPID(held ...cleanup.Holder) []cleanup.Holder {
	slices.SortFunc(held, func(a, b cleanup.Holder) int { return a.PID - b.PID })
	return held
}

type fixture struct {
	tree    string
	real    string
	sub     string
	file    string
	outside string
}

func plant(t *testing.T) fixture {
	t.Helper()
	tree := filepath.Join(t.TempDir(), "tree")
	sub := filepath.Join(tree, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	file := filepath.Join(tree, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return fixture{tree: tree, real: realPath(t, tree), sub: sub, file: file, outside: t.TempDir()}
}

func TestGuard(t *testing.T) {
	ctx := t.Context()
	base := t.TempDir()
	tree := filepath.Join(base, "Tree")
	sub := filepath.Join(tree, "sub")
	sibling := filepath.Join(base, "Tree-2")
	alias := filepath.Join(base, "alias")
	for _, dir := range []string{sub, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	if err := os.Symlink(base, alias); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if got := holders(ctx, t, tree); got != nil {
		t.Fatalf("Guard(empty tree) names %+v, want nil", got)
	}

	neighbor, _ := grandchild(t, sibling, asleep)
	if got := holders(ctx, t, tree); got != nil {
		t.Fatalf("Guard(tree) with pid %d working in %s names %+v, want nil", neighbor, sibling, got)
	}

	holder, stop := grandchild(t, sub, asleep)
	want := []cleanup.Holder{{PID: holder, Name: "sleep", Evidence: cleanup.EvidenceCwd, Path: filepath.Join(realPath(t, tree), "sub")}}
	spellings := []struct {
		name string
		path string
	}{
		{"as registered", tree},
		{"resolved", realPath(t, tree)},
		{"through a symlinked parent", filepath.Join(alias, "Tree")},
		{"in another letter case", filepath.Join(base, "tREE")},
	}
	for _, tt := range spellings {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := os.Stat(tt.path); errors.Is(err, fs.ErrNotExist) {
				t.Skipf("%s does not name the tree on this volume", tt.path)
			}
			if got := holders(ctx, t, tt.path); !reflect.DeepEqual(got, want) {
				t.Errorf("Holders = %+v, want %+v", got, want)
			}
		})
	}

	stop()
	if got := holders(ctx, t, tree); got != nil {
		t.Fatalf("Guard(tree) after its holder exited names %+v, want nil", got)
	}
}

func TestGuardEvidence(t *testing.T) {
	f := plant(t)
	tests := []struct {
		name   string
		dir    string
		script string
		arg    string
		want   *cleanup.Holder
	}{
		{"a working directory", f.sub, asleep, "", &cleanup.Holder{Name: "sleep", Evidence: cleanup.EvidenceCwd, Path: f.real + "/sub"}},
		{"an open file", f.outside, reader, f.file, &cleanup.Holder{Name: "sleep", Evidence: cleanup.EvidenceFD, Path: f.real + "/file"}},
		{"an open directory", f.outside, reader, f.sub, &cleanup.Holder{Name: "sleep", Evidence: cleanup.EvidenceFD, Path: f.real + "/sub"}},
		{"an argument in the kernel's spelling", f.outside, argued, f.real + "/sub", &cleanup.Holder{Name: "bash", Evidence: cleanup.EvidenceArgv, Path: f.real + "/sub"}},
		{"an argument in the caller's spelling", f.outside, argued, f.tree + "/sub", &cleanup.Holder{Name: "bash", Evidence: cleanup.EvidenceArgv, Path: f.tree + "/sub"}},
		{"an argument naming the tree itself", f.outside, argued, f.real, &cleanup.Holder{Name: "bash", Evidence: cleanup.EvidenceArgv, Path: f.real}},
		{"an argument after a flag's equals sign", f.outside, `/bin/bash -c "read line" arg0 "--flag=$0" <&3`, f.real + "/x", &cleanup.Holder{Name: "bash", Evidence: cleanup.EvidenceArgv, Path: f.real + "/x"}},
		{"a command string mentioning the tree", f.outside, `/bin/bash -c "read line; : $0" <&3`, f.real + "/sub", nil},
		{"a relative argument", f.outside, argued, "tree/sub", nil},
		{"an argument naming a sibling that shares the prefix", f.outside, argued, f.real + "-2/sub", nil},
		{"a working directory beside the tree", f.outside, asleep, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args []string
			if tt.arg != "" {
				args = []string{tt.arg}
			}
			pid, _ := grandchild(t, tt.dir, tt.script, args...)
			var want []cleanup.Holder
			if tt.want != nil {
				held := *tt.want
				held.PID = pid
				want = []cleanup.Holder{held}
			}
			if got := holders(t.Context(), t, f.tree); !reflect.DeepEqual(got, want) {
				t.Errorf("Holders = %+v, want %+v", got, want)
			}
		})
	}
}

func TestGuardDiscounts(t *testing.T) {
	f := plant(t)
	arguer, _ := grandchild(t, f.outside, argued, f.real+"/sub")
	both, _ := grandchild(t, f.outside, `/bin/bash -c "read line <&3" arg0 "$0" < "$0"`, f.file)
	opener, _ := grandchild(t, f.outside, reader, f.file)
	worker, _ := grandchild(t, f.sub, asleep)

	argues := cleanup.Holder{PID: arguer, Name: "bash", Evidence: cleanup.EvidenceArgv, Path: f.real + "/sub"}
	bothOpens := cleanup.Holder{PID: both, Name: "bash", Evidence: cleanup.EvidenceFD, Path: f.real + "/file"}
	bothArgues := cleanup.Holder{PID: both, Name: "bash", Evidence: cleanup.EvidenceArgv, Path: f.file}
	opens := cleanup.Holder{PID: opener, Name: "sleep", Evidence: cleanup.EvidenceFD, Path: f.real + "/file"}
	works := cleanup.Holder{PID: worker, Name: "sleep", Evidence: cleanup.EvidenceCwd, Path: f.real + "/sub"}
	exactly := func(pid int) cleanup.ProcessID { return cleanup.ProcessID{PID: pid, Start: startOf(t, pid)} }
	all := []cleanup.Holder{argues, bothOpens, opens, works}

	tests := []struct {
		name      string
		requester cleanup.ProcessID
		retiring  []cleanup.ProcessID
		want      []cleanup.Holder
	}{
		{"nothing named", cleanup.ProcessID{}, nil, all},
		{"the requester's arguments", exactly(arguer), nil, []cleanup.Holder{bothOpens, opens, works}},
		{"not a requester started a second later", cleanup.ProcessID{PID: arguer, Start: startOf(t, arguer) + 1}, nil, all},
		{"not the requester's descriptor", exactly(both), nil, all},
		{"not the requester's only descriptor", exactly(opener), nil, all},
		{"not the requester's working directory", exactly(worker), nil, all},
		{"no arguments but the requester's", exactly(os.Getpid()), nil, all},
		{"a retiring watcher's descriptor", cleanup.ProcessID{}, []cleanup.ProcessID{exactly(opener)}, []cleanup.Holder{argues, bothOpens, works}},
		{"not a watcher started a second later", cleanup.ProcessID{}, []cleanup.ProcessID{{PID: opener, Start: startOf(t, opener) + 1}}, all},
		{"not a watcher started a second earlier", cleanup.ProcessID{}, []cleanup.ProcessID{{PID: opener, Start: startOf(t, opener) - 1}}, all},
		{"not another pid with the watcher's start time", cleanup.ProcessID{}, []cleanup.ProcessID{{PID: os.Getpid(), Start: startOf(t, opener)}}, all},
		{"not a retiring watcher's arguments", cleanup.ProcessID{}, []cleanup.ProcessID{exactly(both)}, []cleanup.Holder{argues, bothArgues, opens, works}},
		{"not a retiring watcher's working directory", cleanup.ProcessID{}, []cleanup.ProcessID{exactly(worker)}, all},
		{"not a retiring watcher's only arguments", cleanup.ProcessID{}, []cleanup.ProcessID{exactly(arguer)}, all},
		{"every retiring watcher named", cleanup.ProcessID{}, []cleanup.ProcessID{exactly(opener), exactly(both)}, []cleanup.Holder{argues, bothArgues, works}},
		{"a requester that is also a retiring watcher", exactly(both), []cleanup.ProcessID{exactly(both)}, []cleanup.Holder{argues, opens, works}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.requester != (cleanup.ProcessID{}) {
				ctx = cleanup.WithRequester(ctx, tt.requester)
			}
			if tt.retiring != nil {
				ctx = cleanup.WithRetiring(ctx, tt.retiring)
			}
			if got, want := holders(ctx, t, f.tree), byPID(slices.Clone(tt.want)...); !reflect.DeepEqual(got, want) {
				t.Errorf("Holders = %+v, want %+v", got, want)
			}
		})
	}
}

func TestGuardDiscountsTheRequestersLaunchers(t *testing.T) {
	f := plant(t)
	wrapper, _ := grandchild(t, f.outside, `/usr/bin/time /bin/bash -c "read line" arg0 "$0" <&3`, f.real+"/sub")
	wrapped := childOf(t, wrapper, "/bin/bash")
	parent, _ := grandchild(t, f.outside, `/bin/bash -c "/bin/sleep 60 & wait" arg0 "$0"`, f.real+"/sub")
	sleeper := childOf(t, parent, "/bin/sleep")
	naming, _ := grandchild(t, f.outside, `/bin/bash -c '/bin/bash "$@" <&3 & wait' "$0" -c "read line" arg0 "$0"`, f.real+"/sub")
	named := childOf(t, naming, "/bin/bash")

	argues := func(pid int, name string) cleanup.Holder {
		return cleanup.Holder{PID: pid, Name: name, Evidence: cleanup.EvidenceArgv, Path: f.real + "/sub"}
	}
	exactly := func(pid int) cleanup.ProcessID { return cleanup.ProcessID{PID: pid, Start: startOf(t, pid)} }
	tests := []struct {
		name      string
		requester cleanup.ProcessID
		want      []cleanup.Holder
	}{
		{"nothing named", cleanup.ProcessID{}, []cleanup.Holder{argues(wrapper, "time"), argues(wrapped, "bash"), argues(parent, "bash"), argues(naming, "bash"), argues(named, "bash")}},
		{"a requester run under timeout", exactly(wrapped), []cleanup.Holder{argues(parent, "bash"), argues(naming, "bash"), argues(named, "bash")}},
		{"not a parent started with other arguments", exactly(sleeper), []cleanup.Holder{argues(wrapper, "time"), argues(wrapped, "bash"), argues(parent, "bash"), argues(naming, "bash"), argues(named, "bash")}},
		{"not a launcher naming the tree before the requester's arguments", exactly(named), []cleanup.Holder{argues(wrapper, "time"), argues(wrapped, "bash"), argues(parent, "bash"), argues(naming, "bash")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.requester != (cleanup.ProcessID{}) {
				ctx = cleanup.WithRequester(ctx, tt.requester)
			}
			if got, want := holders(ctx, t, f.tree), byPID(tt.want...); !reflect.DeepEqual(got, want) {
				t.Errorf("Holders = %+v, want %+v", got, want)
			}
		})
	}
}

func childOf(t *testing.T, parent int, program string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
		if err != nil {
			t.Fatalf("list processes: %v", err)
		}
		for _, p := range procs {
			if int(p.Eproc.Ppid) == parent {
				pid := int(p.Proc.P_pid)
				awaitProgram(t, pid, program)
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never started %s", parent, program)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGuardExemptsItsOwnProcessAndChildren(t *testing.T) {
	f := plant(t)
	worker, _ := grandchild(t, f.sub, asleep)
	want := []cleanup.Holder{{PID: worker, Name: "sleep", Evidence: cleanup.EvidenceCwd, Path: f.real + "/sub"}}
	open := func(t *testing.T) *os.File {
		t.Helper()
		file, err := os.Open(f.file)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = file.Close() })
		return file
	}
	tests := []struct {
		name  string
		start func(t *testing.T)
	}{
		{"its own descriptor", func(t *testing.T) { open(t) }},
		{"a child working in the tree with a file of it open", func(t *testing.T) { spawn(t, f.sub, open(t), "/bin/sleep", "60") }},
		{"a child started with a path in the tree", func(t *testing.T) {
			spawn(t, f.outside, unfed(t), "/bin/bash", "-c", "read line", "arg0", f.real+"/sub")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.start(t)
			if got := holders(t.Context(), t, f.tree); !reflect.DeepEqual(got, want) {
				t.Errorf("Holders = %+v, want %+v", got, want)
			}
		})
	}
}

func TestGuardCountsItsOwnWorkingDirectory(t *testing.T) {
	if tree := os.Getenv(insideHelperEnv); tree != "" {
		fmt.Printf("%s%+v\n", helperPrefix, holders(t.Context(), t, tree))
		return
	}
	f := plant(t)
	pid, got := runHelperIn(t, f.sub, "TestGuardCountsItsOwnWorkingDirectory", insideHelperEnv+"="+f.tree)
	want := fmt.Sprintf("%+v", []cleanup.Holder{{PID: pid, Name: filepath.Base(os.Args[0]), Evidence: cleanup.EvidenceCwd, Path: f.real + "/sub"}})
	if got != want {
		t.Errorf("Guard from a process working in the tree named %s, want %s", got, want)
	}
}

func chdirThread(t *testing.T, dir string) {
	t.Helper()
	handle, err := purego.Dlopen(libSystemPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		t.Fatalf("dlopen: %v", err)
	}
	chdir, err := purego.Dlsym(handle, "pthread_chdir_np")
	if err != nil {
		t.Fatalf("dlsym pthread_chdir_np: %v", err)
	}
	path := append([]byte(dir), 0)
	var pinner runtime.Pinner
	pinner.Pin(&path[0])
	defer pinner.Unpin()
	if failed, errno := call(chdir, uintptr(unsafe.Pointer(&path[0]))); failed != 0 { //nolint:gosec // FFI takes the pinned pointer
		t.Fatalf("pthread_chdir_np(%s): %v", dir, errno)
	}
}

func TestGuardCountsAThreadsWorkingDirectory(t *testing.T) {
	if tree := os.Getenv(threadHelperEnv); tree != "" {
		before := holders(t.Context(), t, tree)
		runtime.LockOSThread()
		chdirThread(t, filepath.Join(tree, "sub"))
		inside := holders(t.Context(), t, tree)
		chdirThread(t, filepath.Dir(tree))
		fmt.Printf("%s%+v then %+v then %+v\n", helperPrefix, before, inside, holders(t.Context(), t, tree))
		return
	}
	f := plant(t)
	pid, got := runHelperIn(t, f.outside, "TestGuardCountsAThreadsWorkingDirectory", threadHelperEnv+"="+f.tree)
	none := []cleanup.Holder(nil)
	held := []cleanup.Holder{{PID: pid, Name: filepath.Base(os.Args[0]), Evidence: cleanup.EvidenceCwd, Path: f.real + "/sub"}}
	if want := fmt.Sprintf("%+v then %+v then %+v", none, held, none); got != want {
		t.Errorf("Guard from a process one of whose threads works in the tree named %s, want %s", got, want)
	}
}

func descend(t *testing.T, depth int) string {
	t.Helper()
	segment := strings.Repeat("d", 200)
	for range depth {
		if err := os.Mkdir(segment, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if err := os.Chdir(segment); err != nil {
			t.Fatalf("Chdir: %v", err)
		}
	}
	return strings.Repeat("/"+segment, depth)
}

func TestGuardSeesPastTheKernelPathBuffer(t *testing.T) {
	if os.Getenv(deepHelperEnv) == "" {
		if got := runHelper(t, "TestGuardSeesPastTheKernelPathBuffer", deepHelperEnv+"=1"); got != "ok" {
			t.Errorf("helper reported %q, want ok", got)
		}
		return
	}
	tree := t.TempDir()
	if err := os.Chdir(tree); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	deep := realPath(t, tree) + descend(t, 6)
	if err := os.MkdirAll("leaf/inner", 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chdir("leaf/inner"); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	holder, _ := grandchild(t, "", asleep)
	if err := os.Chdir("../.."); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	self := cleanup.Holder{PID: os.Getpid(), Name: filepath.Base(os.Args[0]), Evidence: cleanup.EvidenceCwd, Path: deep}
	far := cleanup.Holder{PID: holder, Name: "sleep", Evidence: cleanup.EvidenceCwd, Path: deep + "/leaf/inner"}
	if len(deep) <= len(vnodeInfoPath{}.path) {
		t.Fatalf("this process works %d bytes deep, inside the kernel's %d-byte buffer", len(deep), len(vnodeInfoPath{}.path))
	}

	tests := []struct {
		name string
		path string
		want []cleanup.Holder
	}{
		{"a tree the buffer holds", tree, byPID(self, far)},
		{"a tree deeper than the buffer", "leaf", []cleanup.Holder{far}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := holders(t.Context(), t, tt.path); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Holders = %+v, want %+v", got, tt.want)
			}
		})
	}
	if !t.Failed() {
		fmt.Printf("%sok\n", helperPrefix)
	}
}

func TestGuardSkipsWhatWasUnlinked(t *testing.T) {
	f := plant(t)
	worker, _ := grandchild(t, f.sub, asleep)
	opener, _ := grandchild(t, f.outside, reader, f.file)
	want := byPID(
		cleanup.Holder{PID: worker, Name: "sleep", Evidence: cleanup.EvidenceCwd, Path: f.real + "/sub"},
		cleanup.Holder{PID: opener, Name: "sleep", Evidence: cleanup.EvidenceFD, Path: f.real + "/file"},
	)
	if got := holders(t.Context(), t, f.tree); !reflect.DeepEqual(got, want) {
		t.Fatalf("Holders before anything is removed = %+v, want %+v", got, want)
	}
	for _, path := range []string{f.sub, f.file} {
		if err := os.Remove(path); err != nil {
			t.Fatalf("Remove: %v", err)
		}
	}
	if got := holders(t.Context(), t, f.tree); got != nil {
		t.Errorf("Guard(tree) whose processes hold only what was removed names %+v, want nil", got)
	}
	if err := os.Mkdir(f.sub, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(f.file, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := holders(t.Context(), t, f.tree); got != nil {
		t.Errorf("Guard(tree) with new entries at the removed paths names %+v, want nil", got)
	}
}

func TestGuardOnTheFilesystemRoot(t *testing.T) {
	holder, _ := grandchild(t, t.TempDir(), asleep)
	got := holders(t.Context(), t, "/")
	if !slices.ContainsFunc(got, func(h cleanup.Holder) bool { return h.PID == holder }) {
		t.Errorf("Guard(/) names %d holders but not pid %d", len(got), holder)
	}
}

func TestGuardSortsHolders(t *testing.T) {
	tree := t.TempDir()
	first, _ := grandchild(t, tree, asleep)
	second, _ := grandchild(t, tree, asleep)
	third, _ := grandchild(t, tree, asleep)
	want := []int{first, second, third}
	slices.Sort(want)

	held := holders(t.Context(), t, tree)
	got := make([]int, 0, len(want))
	for _, h := range held {
		got = append(got, h.PID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("holder pids = %v, want %v", got, want)
	}
}

func TestGuardSkipsAZombie(t *testing.T) {
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatalf("loadLibSystem: %v", err)
	}
	tree := t.TempDir()
	exited := exec.Command("/usr/bin/true")
	exited.Dir = tree
	if err := exited.Start(); err != nil {
		t.Fatalf("start true: %v", err)
	}
	t.Cleanup(func() { _ = exited.Wait() })
	pid := exited.Process.Pid
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := pidInfo[procVnodePathInfo](lib, pid, flavorVnodePathInfo)
		if errors.Is(err, unix.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never became a zombie: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	pids, err := lib.listPIDs(procUIDOnly, os.Getuid())
	if err != nil {
		t.Fatalf("listPIDs: %v", err)
	}
	if !slices.Contains(pids, int32(pid)) { //nolint:gosec // a pid fits int32
		t.Fatalf("zombie pid %d is not listed, so the guard never meets it", pid)
	}
	if got := holders(t.Context(), t, tree); got != nil {
		t.Errorf("Guard(tree holding only a zombie) names %+v, want nil", got)
	}
}

func TestGuardCannotTell(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(tree, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	grandchild(t, tree, asleep)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		path string
		want error
	}{
		{"a missing tree", t.Context(), filepath.Join(base, "absent"), fs.ErrNotExist},
		{"a symlink to a held tree", t.Context(), link, unix.ENOTDIR},
		{"a regular file", t.Context(), file, unix.ENOTDIR},
		{"a cancelled context", cancelled, tree, context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Guard(tt.ctx, tt.path)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Guard(%s) = %v, want %v", tt.path, err, tt.want)
			}
			var active *cleanup.ActiveError
			if errors.As(err, &active) {
				t.Errorf("Guard(%s) = %v, want an error that is not a verdict", tt.path, err)
			}
		})
	}
}

func TestListFDs(t *testing.T) {
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatalf("loadLibSystem: %v", err)
	}
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	exited := exec.Command("/usr/bin/true")
	if err := exited.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}

	own, err := lib.listFDs(os.Getpid())
	if err != nil {
		t.Fatalf("listFDs(this process): %v", err)
	}
	if want := (procFDInfo{fd: int32(dir.Fd()), kind: fdTypeVnode}); !slices.Contains(own, want) { //nolint:gosec // a descriptor fits int32
		t.Errorf("listFDs(this process) = %+v, want it to hold %+v", own, want)
	}
	few, full, err := pidList[procFDInfo](lib, os.Getpid(), flavorListFDs, 1)
	if err != nil || len(few) != 1 || !full {
		t.Errorf("pidList(capacity 1) = %d descriptors, full %t, %v; want 1, full true, nil", len(few), full, err)
	}
	if _, err := lib.listFDs(exited.Process.Pid); !errors.Is(err, unix.ESRCH) {
		t.Errorf("listFDs(an exited process) = %v, want ESRCH", err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if _, err := pidInfo[procBSDInfo](lib, exited.Process.Pid, flavorBSDInfo); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("pidInfo(an exited process) = %v, want ESRCH left in errno", err)
	}
	empty := &libSystem{errno: lib.errno, procPIDInfo: answering(t, func(_ int, buf uintptr) (uintptr, unix.Errno) {
		if buf == 0 {
			return 2 * unsafe.Sizeof(procFDInfo{}), 0
		}
		return 0, 0
	})}
	none, err := empty.listFDs(4242)
	if err != nil || len(none) != 0 {
		t.Errorf("listFDs(a process holding no descriptor) = %+v, %v; want none and no error", none, err)
	}
}

func TestIdentify(t *testing.T) {
	if got, err := Identify(os.Getpid()); err != nil || got != (cleanup.ProcessID{PID: os.Getpid(), Start: startOf(t, os.Getpid())}) {
		t.Errorf("Identify(this process) = %+v, %v; want its pid and kernel start", got, err)
	}
	if _, err := Identify(exitedPID(t)); !errors.Is(err, unix.ESRCH) {
		t.Errorf("Identify(an exited process) = %v, want ESRCH", err)
	}
}

func TestProcessUniqueID(t *testing.T) {
	own, err := ProcessUniqueID(os.Getpid())
	if err != nil {
		t.Fatalf("ProcessUniqueID(this process): %v", err)
	}
	if own == 0 {
		t.Error("ProcessUniqueID(this process) = 0, want the kernel's unique id")
	}
	if again, err := ProcessUniqueID(os.Getpid()); err != nil || again != own {
		t.Errorf("ProcessUniqueID(this process) again = %d, %v; want %d", again, err, own)
	}
	if parent, err := ProcessUniqueID(os.Getppid()); err != nil || parent == own {
		t.Errorf("ProcessUniqueID(the parent) = %d, %v; want an id other than this process's %d", parent, err, own)
	}
	if _, err := ProcessUniqueID(exitedPID(t)); !errors.Is(err, unix.ESRCH) {
		t.Errorf("ProcessUniqueID(an exited process) = %v, want ESRCH", err)
	}
}

func TestListPIDsGrowsUntilItFits(t *testing.T) {
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatalf("loadLibSystem: %v", err)
	}
	few, full, err := lib.listPIDsInto(procAllPIDs, 0, 4)
	if err != nil {
		t.Fatalf("listPIDsInto: %v", err)
	}
	if len(few) != 4 || !full {
		t.Fatalf("listPIDsInto(capacity 4) = %d pids, full %t; want 4 pids, full true", len(few), full)
	}
	all, err := lib.listPIDs(procAllPIDs, 0)
	if err != nil {
		t.Fatalf("listPIDs: %v", err)
	}
	if !slices.Contains(all, int32(os.Getpid())) { //nolint:gosec // a pid fits int32
		t.Errorf("listPIDs(all) holds %d pids but not this process, %d", len(all), os.Getpid())
	}
	if !slices.Contains(all, 1) {
		t.Errorf("listPIDs(all) holds %d pids but not launchd", len(all))
	}
}

func TestFSEventsSampler(t *testing.T) {
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatalf("loadLibSystem: %v", err)
	}
	sampler := NewFSEventsSampler()
	first, err := sampler.Sample(t.Context())
	if err != nil {
		t.Fatalf("first Sample: %v", err)
	}
	if first <= 0 {
		t.Fatalf("first Sample = %v, want > 0", first)
	}
	found := sampler.daemon
	path, err := lib.pidPath(found.pid)
	if err != nil {
		t.Fatalf("pidPath(%d): %v", found.pid, err)
	}
	if !strings.HasSuffix(path, "/fseventsd") {
		t.Errorf("cached pid %d runs %s, want fseventsd", found.pid, path)
	}
	if found.uniqueID == 0 || found.coalition == 0 {
		t.Errorf("cached identity = %+v, want a unique id and a coalition", *found)
	}

	time.Sleep(250 * time.Millisecond)
	second, err := sampler.Sample(t.Context())
	if err != nil {
		t.Fatalf("second Sample: %v", err)
	}
	if second < first {
		t.Errorf("second Sample = %v, below the first %v", second, first)
	}
	if sampler.daemon != found {
		t.Errorf("second Sample rescanned the process table though fseventsd is unchanged")
	}
}

func TestFSEventsSamplerRediscovers(t *testing.T) {
	exited := exec.Command("/usr/bin/true")
	if err := exited.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	tests := []struct {
		name  string
		stale func(*fseventsd)
	}{
		{"a replaced process", func(d *fseventsd) { d.uniqueID++ }},
		{"an exited process", func(d *fseventsd) { d.pid = exited.Process.Pid }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sampler := NewFSEventsSampler()
			if _, err := sampler.Sample(t.Context()); err != nil {
				t.Fatalf("first Sample: %v", err)
			}
			want := *sampler.daemon
			stale := sampler.daemon
			tt.stale(stale)
			if _, err := sampler.Sample(t.Context()); err != nil {
				t.Fatalf("Sample over a stale identity: %v", err)
			}
			if sampler.daemon == stale {
				t.Fatalf("Sample kept the stale identity %+v", *stale)
			}
			if *sampler.daemon != want {
				t.Errorf("rediscovered %+v, want %+v", *sampler.daemon, want)
			}
		})
	}
}

func TestFSEventsSamplerCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewFSEventsSampler().Sample(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Sample(cancelled) = %v, want context.Canceled", err)
	}
}

func scheduling(t *testing.T) string {
	t.Helper()
	handle, err := purego.Dlopen(libSystemPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		t.Fatalf("dlopen: %v", err)
	}
	getIOPolicy, err := purego.Dlsym(handle, "getiopolicy_np")
	if err != nil {
		t.Fatalf("dlsym getiopolicy_np: %v", err)
	}
	policy, errno := call(getIOPolicy, iopolTypeDisk, iopolScopeProcess)
	if policy < 0 {
		t.Fatalf("getiopolicy_np: %v", errno)
	}
	band, err := unix.Getpriority(prioDarwinProcess, 0)
	if err != nil {
		t.Fatalf("Getpriority(PRIO_DARWIN_PROCESS): %v", err)
	}
	nice, err := unix.Getpriority(unix.PRIO_PROCESS, 0)
	if err != nil {
		t.Fatalf("Getpriority(PRIO_PROCESS): %v", err)
	}
	return fmt.Sprintf("background=%d disk=%d nice=%d", band, policy, nice)
}

func childPriority(t *testing.T) string {
	t.Helper()
	child := exec.Command("/bin/sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatalf("start a child: %v", err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	out, err := exec.Command("/bin/ps", "-o", "pri=", "-p", strconv.Itoa(child.Process.Pid)).Output()
	if err != nil {
		t.Fatalf("ps the child: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestBand(t *testing.T) {
	if os.Getenv(backgroundHelperEnv) != "" {
		var states []string
		for _, step := range []struct {
			name  string
			enter func() error
		}{{"background", Band{}.Background}, {"foreground", Band{}.Foreground}, {"background", Band{}.Background}} {
			if err := step.enter(); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
			states = append(states, fmt.Sprintf("%s child=%s", scheduling(t), childPriority(t)))
		}
		fmt.Printf("%s%s\n", helperPrefix, strings.Join(states, " | "))
		return
	}
	before, baseline := scheduling(t), childPriority(t)
	want := "background=1 disk=3 nice=0 child=4 | background=0 disk=0 nice=0 child=" + baseline + " | background=1 disk=3 nice=0 child=4"
	if got := runHelper(t, "TestBand", backgroundHelperEnv+"=1"); got != want {
		t.Errorf("child across Background, Foreground, Background: %s, want %s", got, want)
	}
	if after := scheduling(t); after != before {
		t.Errorf("the test process itself changed: %s, was %s", after, before)
	}
}

func TestVerdictReportsUnreadArgumentsOnlyWithoutHolders(t *testing.T) {
	holder := cleanup.Holder{PID: 7, Name: "vim", Evidence: cleanup.EvidenceCwd, Path: "/wt"}
	unread := []string{"native: inspect pid 97627 (binrun): read its arguments: input/output error"}
	var active *cleanup.ActiveError
	if err := verdict("/wt", []cleanup.Holder{holder}, unread); !errors.As(err, &active) || errors.Is(err, cleanup.ErrUnprobed) {
		t.Errorf("verdict with a holder = %v, want the holder alone", err)
	}
	if err := verdict("/wt", nil, unread); !errors.Is(err, cleanup.ErrUnprobed) || !strings.Contains(err.Error(), "pid 97627") {
		t.Errorf("verdict with only unread arguments = %v, want cleanup.ErrUnprobed naming pid 97627", err)
	}
	if err := verdict("/wt", nil, nil); err != nil {
		t.Errorf("verdict with nothing = %v, want nil", err)
	}
}

func TestParentCommand(t *testing.T) {
	parent, command, err := ParentCommand(os.Getpid())
	if err != nil || parent != os.Getppid() || len(command) == 0 {
		t.Errorf("ParentCommand(self) = %d, %q, %v; want pid %d and its arguments", parent, command, err, os.Getppid())
	}
}
