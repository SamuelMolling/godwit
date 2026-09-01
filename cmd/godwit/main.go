// Command godwit is the entry point for the godwit CLI and service.
package main

import (
	"os"

	"github.com/SamuelMolling/godwit/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
