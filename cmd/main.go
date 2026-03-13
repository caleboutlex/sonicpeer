package main

import (
	"fmt"
	"os"

	"sonicpeer/cmd/app"
)

func main() {
	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
