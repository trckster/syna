package main

import (
	"errors"
	"log"
	"os"

	commoncfg "syna/internal/common/config"
)

func main() {
	paths, err := commoncfg.ResolveClientPaths()
	if err != nil {
		log.Fatal(err)
	}
	if err := run(paths, os.Args); err != nil {
		var code exitCode
		if errors.As(err, &code) {
			os.Exit(int(code))
		}
		log.Fatal(err)
	}
}
