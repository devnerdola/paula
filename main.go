package main

import (
	"os"

	"nerdola.dev/x/paula/internal/cmd"
)

func main() {
	os.Exit(cmd.Main(os.Args[1:]))
}
