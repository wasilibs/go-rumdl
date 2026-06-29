package wasm

import _ "embed"

//go:embed memory.wasm
var Memory []byte

//go:embed rumdl.wasm
var Rumdl []byte
