package main

import (
	"errors"
	"fmt"
	"os"

	commoncfg "syna/internal/common/config"
)

func main() {
	paths, err := commoncfg.ResolveClientPaths()
	if err != nil {
		fatal(err)
	}
	if err := run(paths, os.Args); err != nil {
		var code exitCode
		if errors.As(err, &code) {
			os.Exit(int(code))
		}
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %s\n", err)
	os.Exit(1)
}
