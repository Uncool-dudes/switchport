package main

import (
	"fmt"
	"os"
)

// version is set at build time with -ldflags -X.
var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println("devroute", version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: devroute version")
	os.Exit(2)
}
