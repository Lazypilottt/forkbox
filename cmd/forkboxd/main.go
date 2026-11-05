// Command forkboxd is the Forkbox daemon. It owns every microVM on a host and
// serves the gRPC API and HTTP/SSE gateway (built in Task 6).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "forkboxd: not built yet (the daemon arrives in Task 6)")
	os.Exit(1)
}
