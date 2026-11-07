// Command forkboxd is the Forkbox daemon. It owns every sandbox on the
// host and serves the gRPC API on a Unix socket in the data directory, plus
// a small HTTP gateway with server-sent events for Exec.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Lazypilottt/forkbox/internal/api"
	"github.com/Lazypilottt/forkbox/internal/datadir"
	"github.com/Lazypilottt/forkbox/internal/netns"
	"github.com/Lazypilottt/forkbox/internal/sandbox"
	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/versions"
	"github.com/Lazypilottt/forkbox/internal/vm"
)

var version = "0.1.0"

// Paths inside the data directory.
const (
	RunDir     = "run" // 0700: sockets and the lock
	GRPCSocket = "forkboxd.sock"
	HTTPSocket = "forkboxd-http.sock"
	LockFile   = "forkboxd.lock"
	DBFile     = "forkbox.db"
)

func main() {
	if err := run(); err != nil {
		log.SetFlags(0)
		log.Fatalf("forkboxd: %v", err)
	}
}

func run() error {
	dataDir := flag.String("data-dir", datadir.FromEnv(), "data directory")
	versionsFile := flag.String("versions", "", "versions.yaml to use instead of the built-in copy")
	tcp := flag.String("grpc-tcp", "", "also serve gRPC on this TCP address (needs --token-file); off by default")
	httpTCP := flag.String("http-tcp", "", "also serve the HTTP gateway on this TCP address (needs --token-file); off by default")
	tokenFile := flag.String("token-file", "", "file holding the bearer token for the TCP listeners")
	sweep := flag.Duration("ttl-sweep", 10*time.Second, "how often to destroy sandboxes past their TTL")
	pool := flag.String("net-pool", netns.DefaultPool, "IPv4 pool for sandbox uplinks (one /30 each)")
	exposeAddr := flag.String("expose-addr", "127.0.0.1", "host address port forwards listen on")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("forkboxd", version)
		return nil
	}
	logger := log.New(os.Stderr, "forkboxd: ", log.LstdFlags|log.Lmicroseconds)

	v, err := versions.Load(*versionsFile)
	if err != nil {
		return err
	}
	var tok *api.Token
	if *tcp != "" || *httpTCP != "" {
		if *tokenFile == "" {
			return errors.New("--grpc-tcp and --http-tcp need --token-file")
		}
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		if tok, err = api.NewToken(string(b)); err != nil {
			return err
		}
	}

	runDir := filepath.Join(*dataDir, RunDir)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(runDir, 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(runDir, LockFile))
	if err != nil {
		return err
	}
	defer unlock()

	st, err := store.Open(filepath.Join(*dataDir, DBFile))
	if err != nil {
		return err
	}
	defer st.Close()
	vms, err := vm.NewManager(vm.Options{DataDir: *dataDir, FirecrackerVersion: v.Firecracker.Version})
	if err != nil {
		return err
	}
	nm, err := netns.New(*pool, vm.DefaultUID, vm.DefaultGID)
	if err != nil {
		return err
	}
	sbm := sandbox.New(*dataDir, vms, st, nm)
	sbm.Log = logger
	sbm.ExposeAddr = *exposeAddr

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := nm.EnsureHost(ctx); err != nil {
		return err
	}
	logger.Printf("network: pool %s (%d slots), guest DNS to %s", nm.Pool, nm.Slots(), nm.DNS)
	rep, err := sbm.Recover(ctx)
	if err != nil {
		logger.Printf("recover: %v", err)
	}
	if rep != nil {
		logger.Printf("recover: adopted %d running sandboxes %v (resumed %v), reaped %d %v, removed %d orphans %v",
			len(rep.Adopted), rep.Adopted, rep.Resumed, len(rep.Reaped), rep.Reaped, len(rep.Orphans), rep.Orphans)
	}

	srv := &api.Server{B: sbm, ExposeHost: *exposeAddr}
	errc := make(chan error, 4)
	var closers []func()

	grpcLn, err := listenUnix(filepath.Join(runDir, GRPCSocket))
	if err != nil {
		return err
	}
	g := api.NewGRPCServer(srv, nil)
	go func() { errc <- g.Serve(grpcLn) }()
	closers = append(closers, g.GracefulStop)
	logger.Printf("gRPC on unix:%s", grpcLn.Addr())

	httpLn, err := listenUnix(filepath.Join(runDir, HTTPSocket))
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: api.NewHTTPHandler(srv, nil), ReadHeaderTimeout: 10 * time.Second}
	go func() { errc <- ignoreClosed(hs.Serve(httpLn)) }()
	closers = append(closers, func() { hs.Close() })
	logger.Printf("HTTP gateway on unix:%s", httpLn.Addr())

	if *tcp != "" {
		ln, err := net.Listen("tcp", *tcp)
		if err != nil {
			return err
		}
		gt := api.NewGRPCServer(srv, tok)
		go func() { errc <- gt.Serve(ln) }()
		closers = append(closers, gt.GracefulStop)
		logger.Printf("gRPC on tcp:%s (bearer token required)", ln.Addr())
	}
	if *httpTCP != "" {
		ln, err := net.Listen("tcp", *httpTCP)
		if err != nil {
			return err
		}
		ht := &http.Server{Handler: api.NewHTTPHandler(srv, tok), ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- ignoreClosed(ht.Serve(ln)) }()
		closers = append(closers, func() { ht.Close() })
		logger.Printf("HTTP gateway on tcp:%s (bearer token required)", ln.Addr())
	}

	go func() {
		t := time.NewTicker(*sweep)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				done, err := sbm.ExpireOnce(ctx, now)
				if len(done) > 0 {
					logger.Printf("ttl: destroyed %v", done)
				}
				if err != nil {
					logger.Printf("ttl: %v", err)
				}
			}
		}
	}()

	logger.Printf("forkboxd %s ready (Firecracker %s, data %s)", version, v.Firecracker.Version, *dataDir)
	select {
	case <-ctx.Done():
		logger.Printf("shutting down; sandboxes keep running and are adopted on the next start")
	case err := <-errc:
		if err != nil {
			logger.Printf("listener failed: %v", err)
		}
	}
	for _, c := range closers {
		c()
	}
	return nil
}

// listenUnix listens on an owner-only Unix socket. The run directory is
// already 0700; the socket mode is set before anyone could connect.
func listenUnix(path string) (net.Listener, error) {
	_ = os.Remove(path) // a stale socket from a previous run (the lock is ours)
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another forkboxd owns %s (%v)", filepath.Dir(path), err)
	}
	f.Truncate(0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() { f.Close() }, nil
}

func ignoreClosed(err error) error {
	if errors.Is(err, http.ErrServerClosed) || (err != nil && strings.Contains(err.Error(), "use of closed")) {
		return nil
	}
	return err
}
