// Command forkbox-agent runs inside each guest and serves the host over vsock
// (built in Task 3).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "forkbox-agent: not built yet (the guest agent arrives in Task 3)")
	os.Exit(1)
}
