package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
)

// BindRetry is how often the tailnet ingest address is retried. At boot the
// server can start before tailscaled has brought the address up.
const BindRetry = 30 * time.Second

// Run serves the page and ingest until ctx is cancelled.
//
// Loopback listeners must bind or the server exits: without them there is no
// page and no report from this host's own agent. The tailnet one is retried
// in the background instead, because its absence is a problem with the
// tailnet, not with the server — and a server that is down cannot say which
// hosts have gone quiet.
func Run(ctx context.Context, cfg Config) error {
	publicLn, err := net.Listen("tcp", loopback(statuspage.PublicPort))
	if err != nil {
		return err
	}
	ingestLn, err := net.Listen("tcp", loopback(statuspage.IngestPort))
	if err != nil {
		publicLn.Close()
		return err
	}

	var tailnet func(context.Context) (net.Listener, error)
	if cfg.Listen != "" {
		addr := net.JoinHostPort(cfg.Listen, strconv.Itoa(statuspage.IngestPort))
		tailnet = func(ctx context.Context) (net.Listener, error) {
			return listenRetry(ctx, addr, BindRetry, func(a string) (net.Listener, error) { return net.Listen("tcp", a) })
		}
	}
	return serve(ctx, New(cfg, nil, nil), publicLn, ingestLn, tailnet)
}

// serve runs the server on listeners already bound, until ctx is cancelled
// or a listener fails. Split from Run so a test can bind port 0.
func serve(ctx context.Context, s *Server, publicLn, ingestLn net.Listener, tailnet func(context.Context) (net.Listener, error)) error {
	// Everything started here stops with this context, whichever way serve
	// ends: a listener failing must not leave the silence loop or the
	// tailnet retry running into a shutdown.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	public := newHTTPServer(s.PublicHandler())
	ingest := newHTTPServer(s.IngestHandler())

	errc := make(chan error, 3)
	run := func(srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}
	go run(public, publicLn)
	go run(ingest, ingestLn)

	if tailnet != nil {
		go func() {
			if ln, err := tailnet(ctx); err == nil {
				run(ingest, ln)
			}
		}()
	}

	watching := make(chan struct{})
	go func() {
		defer close(watching)
		s.WatchSilence(ctx)
	}()
	logf("serving the page on %s and reports on %s (%d hosts)", publicLn.Addr(), ingestLn.Addr(), len(s.hosts))

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	cancel()

	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	_ = public.Shutdown(shutdownCtx)
	_ = ingest.Shutdown(shutdownCtx)

	// An alert already decided must not die with the process, and one being
	// decided must finish deciding before the queue is drained.
	<-watching
	s.Flush()
	return err
}

// newHTTPServer bounds every phase of a request. The public side is reached
// from the internet through Caddy, and the ingest side by anything on the
// tailnet, so neither may let a slow client hold a connection open.
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
}

func loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// listenRetry binds addr, retrying every interval until it succeeds or ctx
// ends. It logs the first failure and the recovery, not every attempt, so a
// tailnet down overnight is two journal lines rather than a thousand.
func listenRetry(ctx context.Context, addr string, every time.Duration, listen func(string) (net.Listener, error)) (net.Listener, error) {
	failing := false
	for {
		ln, err := listen(addr)
		if err == nil {
			if failing {
				logf("bound %s; reports from other hosts can arrive", addr)
			}
			return ln, nil
		}
		if !failing {
			logf("cannot bind %s, retrying every %s: %v", addr, every, err)
			failing = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}
