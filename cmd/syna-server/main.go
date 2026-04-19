package main

import (
	"os"

	"syna/internal/server/app"
)

func main() {
	os.Exit(app.Main(os.Args, os.Stdout, os.Stderr))
}
