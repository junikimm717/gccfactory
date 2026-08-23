// Command gccfactory builds canadian-cross GCC toolchains for linux-musl.
//
// It is normally invoked through the ./src/gccf shim, which picks the right
// --dist and --qemu-dir.
// Run `gccfactory help` for the full, self-documenting command surface.
package main

import (
	"os"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
