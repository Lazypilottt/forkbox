package agent

import (
	"strconv"
	"strings"
)

// tcpListen is the LISTEN state in /proc/net/tcp and /proc/net/tcp6.
const tcpListen = "0A"

// listeningIn reports whether text, in /proc/net/tcp format, has a socket
// in LISTEN state on port, on any local address. Reading the table instead
// of connecting means WaitPort never disturbs the server it waits for.
func listeningIn(text string, port uint16) bool {
	for i, line := range strings.Split(text, "\n") {
		if i == 0 { // header
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != tcpListen {
			continue
		}
		colon := strings.LastIndexByte(f[1], ':')
		if colon < 0 {
			continue
		}
		p, err := strconv.ParseUint(f[1][colon+1:], 16, 16)
		if err == nil && uint16(p) == port {
			return true
		}
	}
	return false
}
