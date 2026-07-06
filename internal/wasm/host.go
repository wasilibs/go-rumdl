//nolint:revive
package wasm2go

import (
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wasilibs/go-rumdl/internal/wasm/memory"
)

const (
	hostMemoryPages = 65536 // 4 GiB of 64 KiB pages; the mmap reservation cap.

	preopenFD = 3 // first preopened dir; wasi-libc scans upward from here

	// WASI preview1 errno values.
	errnoSuccess     = 0
	errnoAcces       = 2
	errnoBadf        = 8
	errnoExist       = 20
	errnoInval       = 28
	errnoIo          = 29
	errnoIsdir       = 31
	errnoLoop        = 32
	errnoNametoolong = 37
	errnoNoent       = 44
	errnoNotdir      = 54
	errnoNotempty    = 55

	// WASI filetype values.
	filetypeUnknown     = 0
	filetypeBlockDevice = 1
	filetypeCharDevice  = 2
	filetypeDirectory   = 3
	filetypeRegularFile = 4
	filetypeSymlink     = 7

	// path_open oflags.
	oflagCreat     = 1 << 0
	oflagDirectory = 1 << 1
	oflagExcl      = 1 << 2
	oflagTrunc     = 1 << 3

	// fdflags.
	fdflagAppend = 1 << 0

	// rights used to derive the access mode.
	rightFdRead  = 1 << 1
	rightFdWrite = 1 << 6

	// lookupflags.
	lookupSymlinkFollow = 1 << 0

	// clockid values for clock_time_get.
	clockRealtime  = 0
	clockMonotonic = 1
)

// monoStart anchors CLOCK_MONOTONIC readings to the start of the process.
var monoStart = time.Now()

// ExitCode is panicked by proc_exit and recovered by RunModule so the guest's
// exit status becomes the process exit code without terminating the host.
type ExitCode int32

// RunModule runs the module's _start and returns the guest exit code.
func RunModule(m *Module) (code int) {
	defer func() {
		if r := recover(); r != nil {
			if ec, ok := r.(ExitCode); ok {
				code = int(ec)
				return
			}
			panic(r)
		}
	}()
	m.X_start()
	return 0
}

// SetCwd points wasi-libc's __wasilibc_cwd at cwdAbs so relative paths resolve
// against the host working directory. Call after New (data is initialized) and
// before RunModule. See the wazero-based implementation for the rationale; the
// exported global holds the address of the char* pointer variable, so this
// writes a fresh string buffer and stores its address into that variable.
func SetCwd(m *Module, mem *HostMemory, cwdAbs string) {
	guest := filepath.ToSlash(strings.TrimPrefix(cwdAbs, filepath.VolumeName(cwdAbs)))
	buf := append([]byte(guest), 0)
	strAddr := m.Xmalloc(int32(len(buf))) //nolint:gosec // a cwd path length always fits int32
	if strAddr == 0 {
		log.Fatal("malloc returned null for cwd buffer")
	}
	mem.Write(strAddr, buf)
	cwdVarAddr := *m.X__wasilibc_cwd()
	mem.WriteUint32Le(cwdVarAddr, uint32(strAddr)) //nolint:gosec // storing a wasm address as the u32 cwd pointer
}

// HostMemory is a growable shared linear memory for wasm2go.
type HostMemory struct {
	memory.Memory
	waiters sync.Map
}

func NewHostMemory(pages int64) *HostMemory {
	if pages <= 0 {
		pages = initialPages
	}
	if pages > hostMemoryPages {
		pages = hostMemoryPages
	}
	hm := &HostMemory{}
	hm.Max = hostMemoryPages
	// Commit only the initial pages; the guest grows its heap on demand.
	if hm.Memory.Grow(pages, hm.Max) == -1 {
		panic("failed to initialize host memory")
	}
	return hm
}

func (m *HostMemory) Grow(delta, _ int64) int64 { return m.Memory.Grow(delta, m.Max) }
func (m *HostMemory) Waiters() *sync.Map        { return &m.waiters }

func (m *HostMemory) Read(offset, byteCount int32) []byte {
	return m.Buf[offset : offset+byteCount]
}

func (m *HostMemory) ReadUint32Le(offset int32) uint32     { return load32(m.Buf[offset : offset+4]) }
func (m *HostMemory) Write(offset int32, b []byte)         { copy(m.Buf[offset:], b) }
func (m *HostMemory) WriteByte(offset int32, b byte)       { m.Buf[offset] = b } //nolint:govet
func (m *HostMemory) WriteString(offset int32, s string)   { copy(m.Buf[offset:], s) }
func (m *HostMemory) WriteUint32Le(offset int32, v uint32) { store32(m.Buf[offset:offset+4], v) }
func (m *HostMemory) WriteUint64Le(offset int32, v uint64) { store64(m.Buf[offset:offset+8], v) }

// HostEnv provides the imported env.memory.
type HostEnv struct{ memory Memory }

func NewHostEnv(memory Memory) *HostEnv { return &HostEnv{memory: memory} }
func (e *HostEnv) Xmemory() Memory      { return e.memory }

// HostThreads implements wasi.thread-spawn: each thread is a fresh module that
// shares the same linear memory (via env) and runs wasi_thread_start.
type HostThreads struct {
	p1      Xwasi_snapshot_preview1
	rumdl   Xrumdl
	env     Xenv
	nextTID atomic.Int32
}

func NewHostThreads(p1 Xwasi_snapshot_preview1, rumdl Xrumdl, env Xenv) *HostThreads {
	return &HostThreads{p1: p1, rumdl: rumdl, env: env}
}

func (t *HostThreads) Xthread_spawn_6s4jie(startArg int32) int32 {
	tid := t.nextTID.Add(1)
	child := New(t.rumdl, t.p1, t, t.env)
	go child.Xwasi_thread_start(tid, startArg)
	return tid
}

// fdEntry is one entry in the (unsandboxed) file-descriptor table.
type fdEntry struct {
	file        *os.File // nil for std streams
	path        string   // host path (for dirs: base for path_open/readdir)
	isDir       bool
	isPreopen   bool
	preopenName string
}

// HostWASI implements Xwasi_snapshot_preview1 by delegating directly to the
// host OS with no sandboxing: paths resolve against the real filesystem.
type HostWASI struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	mem    *HostMemory
	args   []string
	env    []string

	mu      sync.Mutex
	fds     map[int32]*fdEntry
	freeFDs []int32
	nextFD  int32
}

func NewHostWASI(mem *HostMemory, stdin io.Reader, stdout, stderr io.Writer, args, env []string) *HostWASI {
	w := &HostWASI{
		stdin: stdin, stdout: stdout, stderr: stderr,
		mem: mem, args: args, env: env,
		fds:    make(map[int32]*fdEntry),
		nextFD: preopenFD + 1,
	}
	w.fds[0] = &fdEntry{}
	w.fds[1] = &fdEntry{}
	w.fds[2] = &fdEntry{}
	// The whole host filesystem, preopened as "/". Combined with SetCwd this
	// gives native path resolution.
	w.fds[preopenFD] = &fdEntry{path: string(filepath.Separator), isDir: true, isPreopen: true, preopenName: "/"}
	return w
}

func (w *HostWASI) lookup(fd int32) *fdEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fds[fd]
}

func (w *HostWASI) alloc(e *fdEntry) int32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	var fd int32
	if n := len(w.freeFDs); n > 0 {
		fd = w.freeFDs[n-1]
		w.freeFDs = w.freeFDs[:n-1]
	} else {
		fd = w.nextFD
		w.nextFD++
	}
	w.fds[fd] = e
	return fd
}

func (w *HostWASI) remove(fd int32) *fdEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.fds[fd]
	if e != nil {
		delete(w.fds, fd)
		w.freeFDs = append(w.freeFDs, fd)
	}
	return e
}

// hostPath resolves a guest path (relative to dirfd's preopen/dir) to a host path.
func (w *HostWASI) hostPath(dirfd, ptr, plen int32) (string, int32) {
	e := w.lookup(dirfd)
	if e == nil {
		return "", errnoBadf
	}
	rel := string(w.mem.Read(ptr, plen))
	return filepath.Join(e.path, filepath.FromSlash(rel)), errnoSuccess
}

func errnoFromErr(err error) int32 {
	switch {
	case err == nil:
		return errnoSuccess
	case errors.Is(err, fs.ErrNotExist):
		return errnoNoent
	case errors.Is(err, fs.ErrExist):
		return errnoExist
	case errors.Is(err, fs.ErrPermission):
		return errnoAcces
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		//nolint:exhaustive // syscall.Errno has hundreds of values; only the ones with a distinct WASI errno are mapped, the rest fall through to EIO.
		switch errno {
		case syscall.ENOTDIR:
			return errnoNotdir
		case syscall.EISDIR:
			return errnoIsdir
		case syscall.ENOTEMPTY:
			return errnoNotempty
		case syscall.ELOOP:
			return errnoLoop
		case syscall.ENAMETOOLONG:
			return errnoNametoolong
		}
	}
	return errnoIo
}

func filetypeOf(m fs.FileMode) byte {
	switch {
	case m.IsDir():
		return filetypeDirectory
	case m&fs.ModeSymlink != 0:
		return filetypeSymlink
	case m&fs.ModeCharDevice != 0:
		return filetypeCharDevice
	case m&fs.ModeDevice != 0:
		return filetypeBlockDevice
	case m.IsRegular():
		return filetypeRegularFile
	default:
		return filetypeUnknown
	}
}

func (w *HostWASI) writeFilestat(buf int32, ino uint64, ft byte, size, mtimeNs int64) {
	b := w.mem.Buf[buf : buf+64]
	for i := range b {
		b[i] = 0
	}
	sz := uint64(size)    //nolint:gosec // filestat size field is u64; a file size is non-negative
	mt := uint64(mtimeNs) //nolint:gosec // filestat time fields are u64 nanoseconds
	store64(b[8:], ino)   // ino (dev at 0 left zero)
	b[16] = ft            // filetype
	store64(b[24:], 1)    // nlink
	store64(b[32:], sz)   // size
	store64(b[40:], mt)   // atim
	store64(b[48:], mt)   // mtim
	store64(b[56:], mt)   // ctim
}

// --- args / env ---

func (w *HostWASI) Xargs_sizes_get(argcPtr, bufSizePtr int32) int32 {
	var bufSize uint32
	for _, a := range w.args {
		bufSize += uint32(len(a) + 1) //nolint:gosec // WASI size is u32; an argv/env byte length fits
	}
	w.mem.WriteUint32Le(argcPtr, uint32(len(w.args))) //nolint:gosec // WASI count is u32; argc fits
	w.mem.WriteUint32Le(bufSizePtr, bufSize)
	return errnoSuccess
}

func (w *HostWASI) Xargs_get(argvPtr, bufPtr int32) int32 {
	ptrs := argvPtr
	buf := bufPtr
	for _, a := range w.args {
		w.mem.WriteUint32Le(ptrs, uint32(buf)) //nolint:gosec // buf is a wasm address stored as a u32 pointer
		ptrs += 4
		w.mem.WriteString(buf, a)
		buf += int32(len(a)) //nolint:gosec // buffer offset within wasm memory fits int32
		w.mem.WriteByte(buf, 0)
		buf++
	}
	return errnoSuccess
}

func (w *HostWASI) Xenviron_sizes_get(countPtr, bufSizePtr int32) int32 {
	var bufSize uint32
	for _, e := range w.env {
		bufSize += uint32(len(e) + 1) //nolint:gosec // WASI size is u32; an argv/env byte length fits
	}
	w.mem.WriteUint32Le(countPtr, uint32(len(w.env))) //nolint:gosec // WASI size is u32; an environ byte length fits
	w.mem.WriteUint32Le(bufSizePtr, bufSize)
	return errnoSuccess
}

func (w *HostWASI) Xenviron_get(envPtr, bufPtr int32) int32 {
	ptrs := envPtr
	buf := bufPtr
	for _, e := range w.env {
		w.mem.WriteUint32Le(ptrs, uint32(buf)) //nolint:gosec // buf is a wasm address stored as a u32 pointer
		ptrs += 4
		w.mem.WriteString(buf, e)
		buf += int32(len(e)) //nolint:gosec // buffer offset within wasm memory fits int32
		w.mem.WriteByte(buf, 0)
		buf++
	}
	return errnoSuccess
}

// --- clock ---

func (w *HostWASI) Xclock_time_get(clockID int32, _ int64, timePtr int32) int32 {
	var ns int64
	switch clockID {
	case clockMonotonic:
		ns = int64(time.Since(monoStart))
	case clockRealtime:
		ns = time.Now().UnixNano()
	default:
		// process/thread cputime clocks fall back to wall time.
		ns = time.Now().UnixNano()
	}
	w.mem.WriteUint64Le(timePtr, uint64(ns)) //nolint:gosec // WASI timestamp is u64 nanoseconds
	return errnoSuccess
}

// --- fd operations ---

func (w *HostWASI) Xfd_close(fd int32) int32 {
	switch fd {
	case 0, 1, 2:
		return errnoSuccess
	}
	e := w.remove(fd)
	if e == nil {
		return errnoBadf
	}
	if e.file != nil {
		_ = e.file.Close()
	}
	return errnoSuccess
}

func (w *HostWASI) Xfd_fdstat_get(fd, buf int32) int32 {
	e := w.lookup(fd)
	if e == nil {
		return errnoBadf
	}
	ft := byte(filetypeRegularFile)
	switch {
	case fd == 0 || fd == 1 || fd == 2:
		ft = filetypeCharDevice
	case e.isDir:
		ft = filetypeDirectory
	}
	b := w.mem.Buf[buf : buf+24]
	for i := range b {
		b[i] = 0
	}
	b[0] = ft                           // fs_filetype
	store64(b[8:], 0xffffffffffffffff)  // fs_rights_base (all)
	store64(b[16:], 0xffffffffffffffff) // fs_rights_inheriting (all)
	return errnoSuccess
}

func (w *HostWASI) Xfd_filestat_get(fd, buf int32) int32 {
	if fd == 0 || fd == 1 || fd == 2 {
		w.writeFilestat(buf, 0, filetypeCharDevice, 0, 0)
		return errnoSuccess
	}
	e := w.lookup(fd)
	if e == nil || e.file == nil {
		return errnoBadf
	}
	st, err := e.file.Stat()
	if err != nil {
		return errnoFromErr(err)
	}
	w.writeFilestat(buf, 0, filetypeOf(st.Mode()), st.Size(), st.ModTime().UnixNano())
	return errnoSuccess
}

func (w *HostWASI) Xfd_filestat_set_size(fd int32, size int64) int32 {
	e := w.lookup(fd)
	if e == nil || e.file == nil {
		return errnoBadf
	}
	if err := e.file.Truncate(size); err != nil {
		return errnoFromErr(err)
	}
	return errnoSuccess
}

func (w *HostWASI) Xfd_read(fd, iovs, iovsLen, nreadPtr int32) int32 {
	var r io.Reader
	if fd == 0 {
		r = w.stdin
	} else if e := w.lookup(fd); e != nil && e.file != nil {
		r = e.file
	} else {
		return errnoBadf
	}
	var total uint32
	for i := range iovsLen {
		iov := iovs + i*8
		ptr := w.mem.ReadUint32Le(iov)
		n := w.mem.ReadUint32Le(iov + 4)
		if n == 0 {
			continue
		}
		got, err := r.Read(w.mem.Buf[ptr : ptr+n])
		total += uint32(got)                  //nolint:gosec // bytes read fits u32
		if err == io.EOF || uint32(got) < n { //nolint:gosec // bytes read fits u32
			break
		}
		if err != nil {
			w.mem.WriteUint32Le(nreadPtr, total)
			return errnoIo
		}
	}
	w.mem.WriteUint32Le(nreadPtr, total)
	return errnoSuccess
}

func (w *HostWASI) Xfd_write(fd, iovs, iovsLen, nwrittenPtr int32) int32 {
	var out io.Writer
	switch fd {
	case 1:
		out = w.stdout
	case 2:
		out = w.stderr
	default:
		if e := w.lookup(fd); e != nil && e.file != nil {
			out = e.file
		} else {
			return errnoBadf
		}
	}
	var total uint32
	for i := range iovsLen {
		iov := iovs + i*8
		ptr := w.mem.ReadUint32Le(iov)
		n := w.mem.ReadUint32Le(iov + 4)
		chunk := w.mem.Buf[ptr : ptr+n]
		got, err := out.Write(chunk)
		total += uint32(got) //nolint:gosec // bytes written fits u32
		if err != nil || got != len(chunk) {
			w.mem.WriteUint32Le(nwrittenPtr, total)
			return errnoIo
		}
	}
	w.mem.WriteUint32Le(nwrittenPtr, total)
	return errnoSuccess
}

func (w *HostWASI) Xfd_seek(fd int32, offset int64, whence, newoffsetPtr int32) int32 {
	e := w.lookup(fd)
	if e == nil || e.file == nil {
		return errnoBadf
	}
	// WASI whence (set/cur/end = 0/1/2) matches io.Seek* constants.
	off, err := e.file.Seek(offset, int(whence))
	if err != nil {
		return errnoFromErr(err)
	}
	w.mem.WriteUint64Le(newoffsetPtr, uint64(off)) //nolint:gosec // WASI filedelta/offset is u64
	return errnoSuccess
}

func (w *HostWASI) Xfd_sync(fd int32) int32 {
	switch fd {
	case 0, 1, 2:
		return errnoSuccess
	}
	e := w.lookup(fd)
	if e == nil || e.file == nil {
		return errnoBadf
	}
	if err := e.file.Sync(); err != nil {
		return errnoFromErr(err)
	}
	return errnoSuccess
}

func (w *HostWASI) Xfd_readdir(fd, buf, bufLen int32, cookie int64, bufusedPtr int32) int32 {
	e := w.lookup(fd)
	if e == nil {
		return errnoBadf
	}
	if !e.isDir {
		return errnoNotdir
	}
	// (*os.File).ReadDir returns entries in raw directory order (unlike the
	// sorting os.ReadDir), matching what the native binary's walk observes.
	dir, err := os.Open(e.path)
	if err != nil {
		return errnoFromErr(err)
	}
	des, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return errnoFromErr(err)
	}
	type dentry struct {
		name string
		ft   byte
	}
	list := make([]dentry, 0, len(des)+2)
	list = append(list, dentry{".", filetypeDirectory}, dentry{"..", filetypeDirectory})
	for _, de := range des {
		list = append(list, dentry{de.Name(), filetypeOf(de.Type())})
	}

	var used int32
	limit := bufLen
	writeChunk := func(src []byte) int32 {
		n := int32(len(src)) //nolint:gosec // a dirent chunk length fits int32
		if used+n > limit {
			n = limit - used
		}
		copy(w.mem.Buf[buf+used:buf+used+n], src[:n])
		used += n
		return n
	}
	for i := int(cookie); i < len(list) && used < limit; i++ {
		d := list[i]
		var hdr [24]byte
		store64(hdr[0:], uint64(i+1))          //nolint:gosec // d_next cookie is a small non-negative index
		store64(hdr[8:], 0)                    // d_ino
		store32(hdr[16:], uint32(len(d.name))) //nolint:gosec // d_namlen; a dir entry name fits u32
		hdr[20] = d.ft
		if writeChunk(hdr[:]) < 24 {
			break
		}
		if writeChunk([]byte(d.name)) < int32(len(d.name)) { //nolint:gosec // a dir entry name length fits int32
			break
		}
	}
	w.mem.WriteUint32Le(bufusedPtr, uint32(used)) //nolint:gosec // bytes written, bounded by bufLen
	return errnoSuccess
}

func (w *HostWASI) Xfd_prestat_get(fd, buf int32) int32 {
	e := w.lookup(fd)
	if e == nil || !e.isPreopen {
		return errnoBadf
	}
	w.mem.WriteByte(buf, 0)                                // tag: dir
	w.mem.WriteUint32Le(buf+4, uint32(len(e.preopenName))) //nolint:gosec // WASI size is u32; a preopen name length fits
	return errnoSuccess
}

func (w *HostWASI) Xfd_prestat_dir_name(fd, buf, bufLen int32) int32 {
	e := w.lookup(fd)
	if e == nil || !e.isPreopen {
		return errnoBadf
	}
	name := e.preopenName
	if int(bufLen) < len(name) {
		return errnoInval
	}
	w.mem.WriteString(buf, name)
	return errnoSuccess
}

// --- path operations ---

func (w *HostWASI) Xpath_open(dirfd, dirflags, pathPtr, pathLen, oflags int32, rightsBase, rightsInheriting int64, fdflags, openedFdPtr int32) int32 {
	p, errno := w.hostPath(dirfd, pathPtr, pathLen)
	if errno != errnoSuccess {
		return errno
	}
	var flag int
	switch {
	case oflags&oflagDirectory != 0:
		flag = os.O_RDONLY
	case rightsBase&rightFdRead != 0 && rightsBase&rightFdWrite != 0:
		flag = os.O_RDWR
	case rightsBase&rightFdWrite != 0:
		flag = os.O_WRONLY
	default:
		flag = os.O_RDONLY
	}
	if oflags&oflagCreat != 0 {
		flag |= os.O_CREATE
	}
	if oflags&oflagExcl != 0 {
		flag |= os.O_EXCL
	}
	if oflags&oflagTrunc != 0 {
		flag |= os.O_TRUNC
	}
	if fdflags&fdflagAppend != 0 {
		flag |= os.O_APPEND
	}
	f, err := os.OpenFile(p, flag, 0o644) //nolint:gosec // 0644 matches the native created-file mode (umask applies)
	if err != nil {
		return errnoFromErr(err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return errnoFromErr(err)
	}
	isDir := st.IsDir()
	if oflags&oflagDirectory != 0 && !isDir {
		_ = f.Close()
		return errnoNotdir
	}
	fd := w.alloc(&fdEntry{file: f, path: p, isDir: isDir})
	w.mem.WriteUint32Le(openedFdPtr, uint32(fd)) //nolint:gosec // fd stored as a u32 in guest memory
	return errnoSuccess
}

func (w *HostWASI) Xpath_filestat_get(dirfd, flags, pathPtr, pathLen, buf int32) int32 {
	p, errno := w.hostPath(dirfd, pathPtr, pathLen)
	if errno != errnoSuccess {
		return errno
	}
	var st os.FileInfo
	var err error
	if flags&lookupSymlinkFollow != 0 {
		st, err = os.Stat(p)
	} else {
		st, err = os.Lstat(p)
	}
	if err != nil {
		return errnoFromErr(err)
	}
	w.writeFilestat(buf, 0, filetypeOf(st.Mode()), st.Size(), st.ModTime().UnixNano())
	return errnoSuccess
}

func (w *HostWASI) Xpath_create_directory(dirfd, pathPtr, pathLen int32) int32 {
	p, errno := w.hostPath(dirfd, pathPtr, pathLen)
	if errno != errnoSuccess {
		return errno
	}
	if err := os.Mkdir(p, 0o750); err != nil {
		return errnoFromErr(err)
	}
	return errnoSuccess
}

func (w *HostWASI) Xpath_remove_directory(dirfd, pathPtr, pathLen int32) int32 {
	p, errno := w.hostPath(dirfd, pathPtr, pathLen)
	if errno != errnoSuccess {
		return errno
	}
	st, err := os.Lstat(p)
	if err != nil {
		return errnoFromErr(err)
	}
	if !st.IsDir() {
		return errnoNotdir
	}
	if err := os.Remove(p); err != nil {
		return errnoFromErr(err)
	}
	return errnoSuccess
}

func (w *HostWASI) Xpath_unlink_file(dirfd, pathPtr, pathLen int32) int32 {
	p, errno := w.hostPath(dirfd, pathPtr, pathLen)
	if errno != errnoSuccess {
		return errno
	}
	if err := os.Remove(p); err != nil {
		return errnoFromErr(err)
	}
	return errnoSuccess
}

func (w *HostWASI) Xpath_rename(dirfd, oldPtr, oldLen, newDirfd, newPtr, newLen int32) int32 {
	oldP, errno := w.hostPath(dirfd, oldPtr, oldLen)
	if errno != errnoSuccess {
		return errno
	}
	newP, errno := w.hostPath(newDirfd, newPtr, newLen)
	if errno != errnoSuccess {
		return errno
	}
	if err := os.Rename(oldP, newP); err != nil {
		return errnoFromErr(err)
	}
	return errnoSuccess
}

func (w *HostWASI) Xpath_readlink(dirfd, pathPtr, pathLen, buf, bufLen, bufusedPtr int32) int32 {
	p, errno := w.hostPath(dirfd, pathPtr, pathLen)
	if errno != errnoSuccess {
		return errno
	}
	target, err := os.Readlink(p)
	if err != nil {
		return errnoFromErr(err)
	}
	n := copy(w.mem.Buf[buf:buf+bufLen], target)
	w.mem.WriteUint32Le(bufusedPtr, uint32(n)) //nolint:gosec // bytes copied fits u32
	return errnoSuccess
}

// --- misc ---

func (w *HostWASI) Xrandom_get(buf, bufLen int32) int32 {
	if _, err := rand.Read(w.mem.Buf[buf : buf+bufLen]); err != nil {
		return errnoIo
	}
	return errnoSuccess
}

func (w *HostWASI) Xsched_yield() int32 {
	runtime.Gosched()
	return errnoSuccess
}

func (w *HostWASI) Xproc_exit(code int32) {
	panic(ExitCode(code))
}

var (
	_ Xwasi_snapshot_preview1 = (*HostWASI)(nil)
	_ Xwasi                   = (*HostThreads)(nil)
	_ Xenv                    = (*HostEnv)(nil)
	_ Memory                  = (*HostMemory)(nil)
)
