# go-rumdl

go-rumdl is a distribution of [rumdl][1], that can be built with Go. It does not actually reimplement any
functionality of rumdl in Go, instead building it into a WebAssembly binary, and
executing with the pure Go Wasm runtime [wazero][2]. This means that `go install` or `go run`
can be used to execute it, with no need to rely on separate package managers such as cargo,
on any platform that Go supports.

## Installation

Precompiled binaries are available in the [releases](https://github.com/wasilibs/go-rumdl/releases).
Alternatively, install the plugin you want using `go install`.

```bash
$ go install github.com/wasilibs/go-rumdl/cmd/rumdl@latest
```

To avoid installation entirely, it can be convenient to use `go run`

```bash
$ go run github.com/wasilibs/go-rumdl/cmd/rumdl@latest .
```

Note that due to the sandboxing of the filesystem when using Wasm, currently only files that descend
from the current directory when executing the tool are accessible to it, i.e., `../md/my.md` or
`/separate/root/my.md` will not be found.

[1]: https://github.com/rvben/rumdl
[2]: https://wazero.io/
