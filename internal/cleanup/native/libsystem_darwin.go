package native

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

const (
	libSystemPath = "/usr/lib/libSystem.B.dylib"

	procAllPIDs = 1
	procUIDOnly = 4

	fdFlavorVnodePathInfo = 2
	fdTypeVnode           = 1

	flavorListFDs            = 1
	flavorBSDInfo            = 3
	flavorListThreads        = 6
	flavorVnodePathInfo      = 9
	flavorThreadPathInfo     = 10
	flavorUniqIdentifierInfo = 17
	flavorCoalitionInfo      = 20

	pidPathBytes  = 4096
	longPathBytes = 8192
	noDevice      = 0xffffffff

	vnodeNone = 0

	statusZombie  = 5
	flagInExit    = 4
	flagThreadCwd = 0x100

	threadRoom = 64
)

type vnodeInfoPath struct {
	dev  int32
	_    [4]byte
	ino  uint64
	_    [120]byte
	kind int32
	_    [4]byte
	fsid [2]int32
	path [1024]byte
}

type vnodeFDInfoWithPath struct {
	_     [24]byte
	vnode vnodeInfoPath
}

type procVnodePathInfo struct {
	cdir vnodeInfoPath
	_    vnodeInfoPath
}

type procThreadPathInfo struct {
	_    [112]byte
	cdir vnodeInfoPath
}

type procBSDInfo struct {
	flags  uint32
	status uint32
	_      [8]byte
	ppid   uint32
	_      [28]byte
	comm   [16]byte
	name   [32]byte
	_      [12]byte
	tdev   uint32
	_      [8]byte
	start  uint64
	_      [8]byte
}

type procFDInfo struct {
	fd   int32
	kind uint32
}

type procUniqIdentifierInfo struct {
	_        [16]byte
	uniqueID uint64
	_        [32]byte
}

type procPIDCoalitionInfo struct {
	ids [2]uint64
	_   [3]uint64
}

type coalitionResourceUsage struct {
	_       [3]uint64
	cpuTime uint64
	_       [124]uint64
}

type machTimebase struct {
	numer uint32
	denom uint32
}

type libSystem struct {
	procListPIDs     uintptr
	procPIDInfo      uintptr
	procPIDFDInfo    uintptr
	procPIDPath      uintptr
	fsGetPath        uintptr
	coalitionUsage   uintptr
	machTimebaseInfo uintptr
	setIOPolicy      uintptr
	errno            func() *int32
}

var loadLibSystem = sync.OnceValues(func() (*libSystem, error) {
	handle, err := purego.Dlopen(libSystemPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, fmt.Errorf("native: dlopen %s: %w", libSystemPath, err)
	}
	lib := &libSystem{}
	for name, slot := range map[string]*uintptr{
		"proc_listpids":                 &lib.procListPIDs,
		"proc_pidinfo":                  &lib.procPIDInfo,
		"proc_pidfdinfo":                &lib.procPIDFDInfo,
		"proc_pidpath":                  &lib.procPIDPath,
		"fsgetpath":                     &lib.fsGetPath,
		"coalition_info_resource_usage": &lib.coalitionUsage,
		"mach_timebase_info":            &lib.machTimebaseInfo,
		"setiopolicy_np":                &lib.setIOPolicy,
	} {
		symbol, err := purego.Dlsym(handle, name)
		if err != nil {
			return nil, fmt.Errorf("native: dlsym %s: %w", name, err)
		}
		*slot = symbol
	}
	location, err := purego.Dlsym(handle, "__error")
	if err != nil {
		return nil, fmt.Errorf("native: dlsym __error: %w", err)
	}
	purego.RegisterFunc(&lib.errno, location)
	return lib, nil
})

func call(symbol uintptr, args ...uintptr) (int32, unix.Errno) {
	result, _, errno := purego.SyscallN(symbol, args...)
	// purego's darwin trampoline reads errno on the thread that made the call,
	// but loads the 4-byte C int with an 8-byte move: the upper half is
	// adjacent thread-local noise.
	return int32(result), unix.Errno(uint32(errno)) //nolint:gosec // both are C ints in the low half of a register
}

func (lib *libSystem) callCleared(symbol uintptr, args ...uintptr) (int32, unix.Errno) {
	// A call that may succeed with zero leaves errno as it found it, so only
	// an errno cleared on the same thread beforehand tells that success from a
	// failure. Nothing in libSystem ever sets errno back to zero.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	*lib.errno() = 0
	return call(symbol, args...)
}

func pidInfo[T any](lib *libSystem, pid, flavor int) (*T, error) {
	return pidInfoOf[T](lib, pid, flavor, 0)
}

func pidInfoOf[T any](lib *libSystem, pid, flavor int, subject uint64) (*T, error) {
	out := new(T)
	var pinner runtime.Pinner
	pinner.Pin(out)
	defer pinner.Unpin()
	size := unsafe.Sizeof(*out)
	filled, errno := call(lib.procPIDInfo, uintptr(pid), uintptr(flavor), uintptr(subject), uintptr(unsafe.Pointer(out)), size) //nolint:gosec // FFI takes the pinned pointer; pids and flavors are non-negative
	if filled <= 0 {
		return nil, errno
	}
	if uintptr(filled) != size { //nolint:gosec // filled is positive here
		return nil, fmt.Errorf("kernel filled %d of %d bytes", filled, size)
	}
	return out, nil
}

func pidList[T any](lib *libSystem, pid, flavor, capacity int) (items []T, full bool, err error) {
	buf := make([]T, capacity)
	var pinner runtime.Pinner
	pinner.Pin(&buf[0])
	defer pinner.Unpin()
	width := int(unsafe.Sizeof(buf[0]))
	filled, errno := lib.callCleared(lib.procPIDInfo, uintptr(pid), uintptr(flavor), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(capacity*width)) //nolint:gosec // FFI takes the pinned pointer; pids and flavors are non-negative
	if filled <= 0 && errno != 0 {
		return nil, false, errno
	}
	count := int(filled) / width
	return buf[:count], count == capacity, nil
}

func (lib *libSystem) listPIDs(kind, arg int) ([]int32, error) {
	for capacity := 1024; ; capacity *= 2 {
		pids, full, err := lib.listPIDsInto(kind, arg, capacity)
		if err != nil {
			return nil, err
		}
		if !full {
			return pids, nil
		}
	}
}

func (lib *libSystem) listPIDsInto(kind, arg, capacity int) (pids []int32, full bool, err error) {
	buf := make([]int32, capacity)
	var pinner runtime.Pinner
	pinner.Pin(&buf[0])
	defer pinner.Unpin()
	width := int(unsafe.Sizeof(buf[0]))
	filled, errno := call(lib.procListPIDs, uintptr(kind), uintptr(arg), uintptr(unsafe.Pointer(&buf[0])), uintptr(capacity*width)) //nolint:gosec // FFI takes the pinned pointer; the selector and uid are non-negative
	if filled <= 0 {
		return nil, false, fmt.Errorf("native: list pids: %w", errno)
	}
	count := int(filled) / width
	return buf[:count], count == capacity, nil
}

func (lib *libSystem) listFDs(pid int) ([]procFDInfo, error) {
	room, errno := call(lib.procPIDInfo, uintptr(pid), flavorListFDs, 0, 0, 0) //nolint:gosec // pids are non-negative
	if room <= 0 {
		return nil, errno
	}
	for capacity := int(room) / int(unsafe.Sizeof(procFDInfo{})); ; capacity *= 2 {
		fds, full, err := pidList[procFDInfo](lib, pid, flavorListFDs, capacity)
		if err != nil {
			return nil, err
		}
		if !full {
			return fds, nil
		}
	}
}

func (lib *libSystem) listThreads(pid int) ([]uint64, error) {
	for capacity := threadRoom; ; capacity *= 2 {
		threads, full, err := pidList[uint64](lib, pid, flavorListThreads, capacity)
		if err != nil {
			return nil, err
		}
		if !full {
			return threads, nil
		}
	}
}

func (lib *libSystem) vnodeOfFD(pid int, fd int32) (*vnodeInfoPath, error) {
	info := new(vnodeFDInfoWithPath)
	var pinner runtime.Pinner
	pinner.Pin(info)
	defer pinner.Unpin()
	size := unsafe.Sizeof(*info)
	filled, errno := call(lib.procPIDFDInfo, uintptr(pid), uintptr(fd), fdFlavorVnodePathInfo, uintptr(unsafe.Pointer(info)), size) //nolint:gosec // FFI takes the pinned pointer; pids and descriptors are non-negative
	if filled <= 0 {
		return nil, errno
	}
	if uintptr(filled) != size { //nolint:gosec // filled is positive here
		return nil, fmt.Errorf("kernel filled %d of %d bytes", filled, size)
	}
	return &info.vnode, nil
}

func (lib *libSystem) pidPath(pid int) (string, error) {
	buf := make([]byte, pidPathBytes)
	var pinner runtime.Pinner
	pinner.Pin(&buf[0])
	defer pinner.Unpin()
	length, errno := call(lib.procPIDPath, uintptr(pid), uintptr(unsafe.Pointer(&buf[0])), pidPathBytes) //nolint:gosec // FFI takes the pinned pointer; pids are non-negative
	if length <= 0 {
		return "", errno
	}
	return string(buf[:length]), nil
}

func (lib *libSystem) pathByID(fsid [2]int32, ino uint64) (string, error) {
	buf := make([]byte, longPathBytes)
	var pinner runtime.Pinner
	pinner.Pin(&buf[0])
	pinner.Pin(&fsid)
	defer pinner.Unpin()
	length, errno := call(lib.fsGetPath, uintptr(unsafe.Pointer(&buf[0])), longPathBytes, uintptr(unsafe.Pointer(&fsid)), uintptr(ino)) //nolint:gosec // FFI takes the pinned pointers
	if length <= 0 {
		return "", errno
	}
	return unix.ByteSliceToString(buf[:length]), nil
}
