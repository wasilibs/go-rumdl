package main

import (
	"github.com/curioswitch/go-build"
	"github.com/goyek/x/boot"
	"github.com/wasilibs/tools/tasks"
)

func main() {
	tasks.Define(tasks.Params{
		LibraryName: "rumdl",
		LibraryRepo: "rvben/rumdl",
		BuildOpts:   []build.Option{build.DisableCoverage()},
	})
	boot.Main()
}
