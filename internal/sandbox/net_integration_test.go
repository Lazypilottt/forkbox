//go:build integration

package sandbox

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/netns"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

func hostTable(t *testing.T) string {
	out, err := exec.Command("nft", "list", "table", "ip", "fbx").CombinedOutput()
	if err != nil {
		t.Fatalf("nft list table ip fbx: %v %s", err, out)
	}
	return string(out)
}

func fbxLinks() []string {
	ents, _ := os.ReadDir("/sys/class/net")
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), netns.Prefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestIntegrationNetworkTwenty(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	tableBefore := hostTable(t)

	const n = 20
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			m.Destroy(context.Background(), id)
		}
	})
	for i := 0; i < n; i++ {
		c, err := m.Create(ctx, CreateOptions{Template: "python"})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, c.ID)
		if i == 0 {
			t.Logf("create with network: %+v", c.Timings)
		}
	}

	// Watch the host side of every uplink for HTTPS from the pool.
	dump := exec.Command("tcpdump", "-i", "any", "-nn", "-l", "-q", "src net "+m.Net.Pool.String()+" and tcp dst port 443 and tcp[tcpflags] & tcp-syn != 0")
	out, err := dump.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	dump.Stderr = io.Discard
	if err := dump.Start(); err != nil {
		t.Fatalf("tcpdump: %v (apt install tcpdump)", err)
	}
	sources := map[string]bool{}
	done := make(chan struct{})
	srcRE := regexp.MustCompile(`IP (\d+\.\d+\.\d+\.\d+)\.\d+ > `)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if m := srcRE.FindStringSubmatch(sc.Text()); m != nil {
				sources[m[1]] = true
			}
		}
		close(done)
	}()
	time.Sleep(time.Second) // let tcpdump attach

	for _, id := range ids {
		r, err := m.Agent(id).Run(ctx, agentclient.ExecRequest{Args: []string{"curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}",
			"--max-time", "20", "https://example.com/"}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if r.Code != 0 || string(r.Stdout) != "200" {
			t.Errorf("%s: curl exit %d, status %q, stderr %q", id, r.Code, r.Stdout, r.Stderr)
		}
	}
	time.Sleep(time.Second)
	dump.Process.Signal(os.Interrupt)
	<-done
	dump.Wait()
	t.Logf("%d distinct source addresses on the host: %v", len(sources), keys(sources))
	if len(sources) != n {
		t.Errorf("want %d distinct sources, saw %d", n, len(sources))
	}

	// Other traffic is not allowed out yet (only DNS, HTTP, HTTPS, ping).
	r, _ := m.Agent(ids[0]).Run(ctx, agentclient.ExecRequest{Args: []string{"curl", "-sS", "--max-time", "5", "telnet://example.com:25"}}, 0)
	if r.Code == 0 {
		t.Errorf("port 25 reached the internet")
	}
	r, _ = m.Agent(ids[0]).Run(ctx, agentclient.ExecRequest{Args: []string{"curl", "-sS", "--max-time", "3", "http://169.254.169.254/"}}, 0)
	if r.Code == 0 {
		t.Errorf("a sandbox reached the cloud metadata address")
	}

	// A forwarded port serves a guest HTTP server.
	ac := m.Agent(ids[0])
	if _, err := ac.WriteFile(ctx, "/srv/hello.txt", strings.NewReader("hello from "+ids[0]+"\n"), agentclient.WriteOptions{MakeParents: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Run(ctx, agentclient.ExecRequest{Args: []string{"sh", "-c", "setsid python -m http.server 8000 --directory /srv >/dev/null 2>&1 < /dev/null &"}}, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := ac.WaitPort(ctx, 8000, 20*time.Second); err != nil || !ok {
		t.Fatalf("guest server not listening: %v %v", ok, err)
	}
	port, err := m.Expose(ctx, ids[0], 8000, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello from "+ids[0]+"\n" {
		t.Errorf("forwarded port served %q", body)
	}
	t.Logf("guest port 8000 served on 127.0.0.1:%d", port)

	for _, id := range ids {
		if err := m.Destroy(ctx, id); err != nil {
			t.Errorf("destroy %s: %v", id, err)
		}
	}
	ids = nil
	if left := netns.Namespaces(); len(left) != 0 {
		t.Errorf("namespaces left: %v", left)
	}
	if left := fbxLinks(); len(left) != 0 {
		t.Errorf("links left: %v", left)
	}
	if after := hostTable(t); after != tableBefore {
		t.Errorf("host rules changed:\nbefore:\n%s\nafter:\n%s", tableBefore, after)
	}
	if _, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/"); err == nil {
		t.Errorf("port forward outlived its sandbox")
	}
	m2, _ := filepath.Glob("/run/netns/fbx-*")
	if len(m2) != 0 {
		t.Errorf("netns handles left: %v", m2)
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
