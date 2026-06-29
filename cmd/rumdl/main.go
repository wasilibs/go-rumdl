package main

import (
	"os"

	"github.com/wasilibs/go-rumdl/internal/runner"
)

func main() {
	os.Exit(runner.Run("rumdl", os.Args[1:], os.Stdin, os.Stdout, os.Stderr, "."))
}
