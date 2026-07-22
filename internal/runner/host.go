package runner

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"go/format"
	"os/exec"
	"strings"
	"time"

	wasm2go "github.com/wasilibs/go-rumdl/internal/wasm"
)

// HostRumdl implements the "rumdl" import module (wasm2go.Xrumdl) that the
// guest calls to run code-block formatters/linters via host process spawning.
// The guest lacks process support under WASI, so it delegates here.
type HostRumdl struct {
	mem *wasm2go.HostMemory
	m   *wasm2go.Module // set via SetModule after New, for rumdl_wasm_alloc
}

func NewHostRumdl(mem *wasm2go.HostMemory) *HostRumdl {
	return &HostRumdl{mem: mem}
}

// SetModule wires up the module whose exported allocator returns output
// buffers. Call after New and before the guest runs.
func (h *HostRumdl) SetModule(m *wasm2go.Module) { h.m = m }

// Xcheck_tool_exists(name_ptr, name_len) -> i32 (0/1).
//
//nolint:revive // method name is dictated by the wasm2go Xrumdl import interface.
func (h *HostRumdl) Xcheck_tool_exists(namePtr, nameLen int32) int32 {
	name := string(h.mem.Read(namePtr, nameLen))
	if toolExists(name) {
		return 1
	}
	return 0
}

// Xexecute_tool(name, args, stdin, timeout, →stdout, →stderr) -> exit_code.
//
//nolint:revive // method name is dictated by the wasm2go Xrumdl import interface.
func (h *HostRumdl) Xexecute_tool(namePtr, nameLen, argsPtr, argsLen, stdinPtr, stdinLen int32, timeoutMs int64, outStdoutPtr, outStdoutLen, outStderrPtr, outStderrLen int32) int32 {
	name := string(h.mem.Read(namePtr, nameLen))
	args := decodeArgs(h.readBytes(argsPtr, argsLen))
	stdin := h.readBytes(stdinPtr, stdinLen)

	stdout, stderr, exitCode := runTool(context.Background(), name, args, stdin, uint64(timeoutMs)) //nolint:gosec // guest passes a non-negative timeout as u64

	h.writeOutput(stdout, outStdoutPtr, outStdoutLen)
	h.writeOutput(stderr, outStderrPtr, outStderrLen)
	return int32(exitCode) //nolint:gosec // exit codes are small
}

// readBytes copies a [ptr, len) range out of guest memory. Read returns a view
// aliasing linear memory; copying insulates callers from a later Grow (which
// reallocates the backing slice) or an allocator write.
func (h *HostRumdl) readBytes(ptr, length int32) []byte {
	if length == 0 {
		return nil
	}
	out := make([]byte, length)
	copy(out, h.mem.Read(ptr, length))
	return out
}

// writeOutput allocates guest memory via the exported allocator, copies data
// into it, and stores the resulting pointer and length at the out-param
// addresses. A zero-length payload writes a null pointer and zero length.
func (h *HostRumdl) writeOutput(data []byte, outPtrAddr, outLenAddr int32) {
	if len(data) == 0 {
		h.mem.WriteUint32Le(outPtrAddr, 0)
		h.mem.WriteUint32Le(outLenAddr, 0)
		return
	}
	ptr := h.m.Xrumdl_wasm_alloc(int32(len(data))) //nolint:gosec // a tool-output length fits int32
	if ptr == 0 {
		panic("rumdl host: rumdl_wasm_alloc returned null")
	}
	h.mem.Write(ptr, data)
	h.mem.WriteUint32Le(outPtrAddr, uint32(ptr))       //nolint:gosec // wasm address stored as u32
	h.mem.WriteUint32Le(outLenAddr, uint32(len(data))) //nolint:gosec // an output length fits u32
}

// Tools we recognize to execute with `go run` to avoid user installation.
var goRunTools = map[string]string{
	"prettier":   "github.com/wasilibs/go-prettier/v3/cmd/prettier@" + verGoPrettier,
	"shellcheck": "github.com/wasilibs/go-shellcheck/cmd/shellcheck@" + verGoShellcheck,
	"goimports":  "golang.org/x/tools/cmd/goimports@" + verGoTools,
	"shfmt":      "mvdan.cc/sh/v3/cmd/shfmt@" + verShfmt,
	"yamlfmt":    "github.com/google/yamlfmt/cmd/yamlfmt@" + verYamlfmt,
}

// toolExists reports whether a tool can be run.
func toolExists(name string) bool {
	name = normalizeToolName(name)
	if name == "gofmt" {
		return true
	}
	if _, ok := goRunTools[name]; ok {
		_, err := exec.LookPath("go")
		return err == nil
	}
	_, err := exec.LookPath(name)
	return err == nil
}

// runTool runs a tool over stdin, returning its stdout, stderr, and exit code.
func runTool(ctx context.Context, name string, args []string, stdin []byte, timeoutMs uint64) (stdout, stderr []byte, exitCode int) {
	name = normalizeToolName(name)
	if name == "gofmt" {
		// We can always run this in-process.
		out, err := format.Source(stdin)
		if err != nil {
			return nil, []byte(err.Error()), 1
		}
		return out, nil, 0
	}

	if timeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond) //nolint:gosec // timeout fits
		defer cancel()
	}

	var cmd *exec.Cmd
	if spec, ok := goRunTools[name]; ok {
		cmd = exec.CommandContext(ctx, "go", append([]string{"run", spec}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, name, args...)
	}
	cmd.Stdin = bytes.NewReader(stdin)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			// Failed to start (e.g. not found): surface the error as stderr.
			return nil, []byte(err.Error()), 1
		}
	}
	if code == 0 {
		if _, ok := goRunTools[name]; ok {
			errBuf = *bytes.NewBuffer(stripGoRunBootstrapStderr(errBuf.Bytes()))
		}
	}
	return outBuf.Bytes(), errBuf.Bytes(), code
}

func normalizeToolName(name string) string {
	base, _, ok := strings.Cut(name, ":")
	if !ok {
		return name
	}
	if _, ok := goRunTools[base]; ok {
		return base
	}
	return name
}

// decodeArgs parses the length-prefixed argument buffer produced by the guest:
// each argument is a little-endian u32 length followed by that many bytes.
func decodeArgs(buf []byte) []string {
	var args []string
	for len(buf) >= 4 {
		n := binary.LittleEndian.Uint32(buf)
		buf = buf[4:]
		if uint32(len(buf)) < n {
			break
		}
		args = append(args, string(buf[:n]))
		buf = buf[n:]
	}
	return args
}

func stripGoRunBootstrapStderr(stderr []byte) []byte {
	if len(stderr) == 0 {
		return nil
	}

	var filtered bytes.Buffer
	for _, line := range bytes.SplitAfter(stderr, []byte("\n")) {
		trimmed := bytes.TrimRight(line, "\r\n")
		if bytes.HasPrefix(trimmed, []byte("go: downloading ")) {
			continue
		}
		filtered.Write(line)
	}

	return bytes.TrimLeft(filtered.Bytes(), "\r\n")
}

var _ wasm2go.Xrumdl = (*HostRumdl)(nil)
