package main

import (
	"github.com/goyek/x/boot"
	"github.com/wasilibs/tools/tasks"
)

func main() {
	tasks.Define(tasks.Params{
		LibraryName: "rumdl",
		LibraryRepo: "rvben/rumdl",
		GoReleaser:  true,
	})
	boot.Main()
}
