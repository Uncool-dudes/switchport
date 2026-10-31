// lookup resolves names through Go's default resolver.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	for _, n := range os.Args[1:] {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		a, err := net.DefaultResolver.LookupHost(ctx, n)
		cancel()
		fmt.Printf("%-34s %v %v\n", n, a, err)
	}
}
