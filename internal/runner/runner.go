package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
	"github.com/wasilibs/wazero-helpers/allocator"

	"github.com/wasilibs/go-rumdl/internal/wasm"
)

func Run(name string, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, cwd string) int {
	ctx := context.Background()

	ctx = experimental.WithMemoryAllocator(ctx, allocator.NewNonMoving())

	rtCfg := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesThreads).
		WithMemoryLimitPages(uint32(65536))
	uc, err := os.UserCacheDir()
	if err == nil {
		cache, err := wazero.NewCompilationCacheWithDir(filepath.Join(uc, "com.github.wasilibs"))
		if err == nil {
			rtCfg = rtCfg.WithCompilationCache(cache)
		}
	}
	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)

	code, err := rt.CompileModule(ctx, wasm.Rumdl)
	if err != nil {
		panic(err)
	}

	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	if _, err := rt.InstantiateWithConfig(ctx, wasm.Memory, wazero.NewModuleConfig().WithName("env")); err != nil {
		log.Fatal(err)
	}

	if err := instantiateHost(ctx, rt); err != nil {
		log.Fatal(err)
	}

	args = append([]string{name}, args...)

	var nextTID atomic.Uint64

	cfg := wazero.NewModuleConfig().
		WithSysNanosleep().
		WithSysNanotime().
		WithSysWalltime().
		WithStderr(stderr).
		WithStdout(stdout).
		WithStdin(stdin).
		WithRandSource(rand.Reader).
		WithArgs(args...).
		WithFSConfig(wazero.NewFSConfig().
			WithDirMount(cwd, "/"))
	for _, env := range os.Environ() {
		k, v, _ := strings.Cut(env, "=")
		cfg = cfg.WithEnv(k, v)
	}

	_, err = rt.NewHostModuleBuilder("wasi").NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, _ api.Module, stack []uint64) {
			tid := nextTID.Add(1)
			startArg := stack[0]
			child, err := rt.InstantiateModule(ctx, code, cfg.
				// Don't need to execute start functions again in child, it crashes anyways.
				WithStartFunctions().
				WithName(""))
			if err != nil {
				panic(err)
			}
			go func() {
				defer func() {
					_ = child.Close(ctx)
				}()
				// wasi_thread_start(thread_id: i32, start_arg: i32)
				if _, err := child.ExportedFunction("wasi_thread_start").Call(ctx, tid, startArg); err != nil {
					log.Printf("wasi_thread_start (tid %d): %v", tid, err)
				}
			}()
			stack[0] = tid
		}), []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("thread-spawn").
		Instantiate(ctx)
	if err != nil {
		log.Fatal(err)
	}

	_, err = rt.InstantiateModule(ctx, code, cfg)
	if err != nil {
		sErr := &sys.ExitError{}
		if errors.As(err, &sErr) {
			return int(sErr.ExitCode())
		}
		log.Fatal(err)
	}
	return 0
}
