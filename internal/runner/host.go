package runner

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// instantiateHost wires up the "rumdl" host module that the guest imports to
// run code-block formatters/linters for process spawning.
func instantiateHost(ctx context.Context, rt wazero.Runtime) error {
	_, err := rt.NewHostModuleBuilder("rumdl").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(checkToolExists),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI32}).
		Export("check_tool_exists").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(executeTool),
			[]api.ValueType{
				api.ValueTypeI32, api.ValueTypeI32, // name ptr, len
				api.ValueTypeI32, api.ValueTypeI32, // args ptr, len
				api.ValueTypeI32, api.ValueTypeI32, // stdin ptr, len
				api.ValueTypeI64,                   // timeout ms (0 = none)
				api.ValueTypeI32, api.ValueTypeI32, // out stdout ptr, len
				api.ValueTypeI32, api.ValueTypeI32, // out stderr ptr, len
			},
			[]api.ValueType{api.ValueTypeI32}).
		Export("execute_tool").
		Instantiate(ctx)
	if err != nil {
		return fmt.Errorf("rumdl host: instantiating module: %w", err)
	}
	return nil
}

// checkToolExists(name_ptr, name_len) -> i32 (0/1).
func checkToolExists(_ context.Context, mod api.Module, stack []uint64) {
	name := readString(mod, uint32(stack[0]), uint32(stack[1]))
	stack[0] = boolToU64(toolExists(name))
}

// executeTool(name, args, stdin, →stdout, →stderr) -> exit_code.
func executeTool(ctx context.Context, mod api.Module, stack []uint64) {
	name := readString(mod, uint32(stack[0]), uint32(stack[1]))
	argsBuf := readBytes(mod, uint32(stack[2]), uint32(stack[3]))
	stdin := readBytes(mod, uint32(stack[4]), uint32(stack[5]))
	timeoutMs := stack[6]

	args := decodeArgs(argsBuf)

	stdout, stderr, exitCode := runTool(ctx, name, args, stdin, timeoutMs)

	writeOutput(ctx, mod, stdout, uint32(stack[7]), uint32(stack[8]))
	writeOutput(ctx, mod, stderr, uint32(stack[9]), uint32(stack[10]))
	stack[0] = uint64(uint32(exitCode))
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
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
		defer cancel()
	}

	// Workaround go-prettier not supporting stdin input yet.
	if name == "prettier" {
		return runPrettier(ctx, args, stdin)
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
	return outBuf.Bytes(), errBuf.Bytes(), code
}

// TODO: once a go-prettier release supports `--stdin-filepath` as a stdin
// filter, drop this shim and the `name == "prettier"` branch above.
func runPrettier(ctx context.Context, args []string, stdin []byte) (stdout, stderr []byte, exitCode int) {
	ext := ".md"
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--stdin-filepath="); ok {
			if e := filepath.Ext(v); e != "" {
				ext = e
			}
		}
	}

	tmp, err := os.MkdirTemp("", "rumdl-prettier-")
	if err != nil {
		return nil, []byte(err.Error()), 1
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	name := "code" + ext
	file := filepath.Join(tmp, name)
	if err := os.WriteFile(file, stdin, 0o600); err != nil {
		return nil, []byte(err.Error()), 1
	}

	// go-prettier globs patterns relative to its working directory
	cmd := exec.CommandContext(ctx, "go", "run", goRunTools["prettier"], "--write", "--no-config", "--no-editorconfig", name)
	cmd.Dir = tmp
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		code := 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		return nil, errBuf.Bytes(), code
	}

	out, err := os.ReadFile(file)
	if err != nil {
		return nil, []byte(err.Error()), 1
	}
	return out, errBuf.Bytes(), 0
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

func readString(mod api.Module, ptr, length uint32) string {
	return string(readBytes(mod, ptr, length))
}

func readBytes(mod api.Module, ptr, length uint32) []byte {
	if length == 0 {
		return nil
	}
	buf, ok := mod.Memory().Read(ptr, length)
	if !ok {
		panic("rumdl host: memory read out of bounds")
	}
	// Read returns a view into linear memory; copy so later writes can't alias.
	out := make([]byte, length)
	copy(out, buf)
	return out
}

// writeOutput allocates guest memory via the exported allocator, copies data
// into it, and stores the resulting pointer and length at the out-param
// addresses. A zero-length payload writes a null pointer and zero length.
func writeOutput(ctx context.Context, mod api.Module, data []byte, outPtrAddr, outLenAddr uint32) {
	if len(data) == 0 {
		writeU32(mod, outPtrAddr, 0)
		writeU32(mod, outLenAddr, 0)
		return
	}
	res, err := mod.ExportedFunction("rumdl_wasm_alloc").Call(ctx, uint64(len(data)))
	if err != nil {
		panic(err)
	}
	ptr := uint32(res[0])
	if !mod.Memory().Write(ptr, data) {
		panic("rumdl host: memory write out of bounds")
	}
	writeU32(mod, outPtrAddr, ptr)
	writeU32(mod, outLenAddr, uint32(len(data)))
}

func writeU32(mod api.Module, addr, val uint32) {
	if !mod.Memory().WriteUint32Le(addr, val) {
		panic("rumdl host: out-param write out of bounds")
	}
}

func boolToU64(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}
