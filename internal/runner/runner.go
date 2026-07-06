package runner

import (
	"io"
	"log"
	"os"
	"path/filepath"

	wasm2go "github.com/wasilibs/go-rumdl/internal/wasm"
)

func Run(name string, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, cwd string) int {
	cwdAbs, err := filepath.Abs(cwd)
	if err != nil {
		log.Fatal(err)
	}

	// Shared linear memory backs the main module and every spawned thread.
	mem := wasm2go.NewHostMemory(0)
	env := wasm2go.NewHostEnv(mem)
	wasi := wasm2go.NewHostWASI(mem, stdin, stdout, stderr, append([]string{name}, args...), os.Environ())
	rumdl := NewHostRumdl(mem)
	threads := wasm2go.NewHostThreads(wasi, rumdl, env)

	// New runs the module's data-init start function; set the working directory
	// before _start so relative, "../", and absolute paths resolve against the
	// host cwd exactly as the native binary does (see wasm2go.SetCwd). The
	// module's exported allocator is wired into the rumdl host afterward so it
	// can return tool output into guest memory.
	m := wasm2go.New(env, wasi, rumdl, threads)
	rumdl.SetModule(m)
	wasm2go.SetCwd(m, mem, cwdAbs)
	return wasm2go.RunModule(m)
}
