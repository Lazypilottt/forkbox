package agent

import "testing"

// From a Linux guest: sshd on 0.0.0.0:22 and a dev server on 127.0.0.1:8080
// listening, an established connection to port 5432, a TIME_WAIT on 3000.
const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 18842 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 20011 1 0000000000000000 100 0 0 10 0
   2: 0100007F:9C40 0100007F:1538 01 00000000:00000000 00:00000000 00000000     0        0 20999 1 0000000000000000 20 4 30 10 -1
   3: 0100007F:0BB8 0100007F:A000 06 00000000:00000000 03:00000F7B 00000000     0        0 0 3 0000000000000000
`

const procNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:1BBC 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 31337 1 0000000000000000 100 0 0 10 0
`

func TestListeningIn(t *testing.T) {
	for port, want := range map[uint16]bool{22: true, 8080: true, 5432: false, 40000: false, 3000: false, 0: false} {
		if got := listeningIn(procNetTCP, port); got != want {
			t.Errorf("tcp port %d: got %v, want %v", port, got, want)
		}
	}
	if !listeningIn(procNetTCP6, 7100) || listeningIn(procNetTCP6, 22) {
		t.Errorf("tcp6 parsing wrong")
	}
	if listeningIn("", 22) || listeningIn("garbage\nmore garbage", 22) {
		t.Errorf("garbage parsed as listening")
	}
}
