package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/journal"
	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
)

// BindRetry is how often the tailnet ingest address is retried. At boot the
// server can start before tailscaled has brought the address up.
const BindRetry = 30 * time.Second

// Run serves the page and ingest until ctx is cancelled.
//
// The page and ingest loopback listeners must bind or the server exits:
// without them there is no page and no report from this host's own agent.
// The tailnet one is retried in the background instead, because its absence
// is a problem with the tailnet, not with the server — and a server that is
// down cannot say which hosts have gone quiet. The local view is only for
// doctor, so failing to bind it is logged and nothing more.
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
	localLn, err := net.Listen("tcp", loopback(statuspage.LocalPort))
	if err != nil {
		slog.Error("cannot bind the host view, so pilot doctor cannot name silent hosts", "err", err)
		localLn = nil
	}

	var tailnet func(context.Context) (net.Listener, error)
	if cfg.Listen != "" {
		addr := net.JoinHostPort(cfg.Listen, strconv.Itoa(statuspage.IngestPort))
		tailnet = func(ctx context.Context) (net.Listener, error) {
			return listenRetry(ctx, addr, BindRetry, func(a string) (net.Listener, error) { return net.Listen("tcp", a) })
		}
	}
	return serve(ctx, New(cfg, nil, nil), publicLn, ingestLn, localLn, tailnet)
}

// serve runs the server on listeners already bound, until ctx is cancelled
// or a listener fails. Split from Run so a test can bind port 0. A nil
// localLn serves no host view.
func serve(ctx context.Context, s *Server, publicLn, ingestLn, localLn net.Listener, tailnet func(context.Context) (net.Listener, error)) error {
	// Everything started here stops with this context, whichever way serve
	// ends: a listener failing must not leave the silence loop or the
	// tailnet retry running into a shutdown.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	public := newHTTPServer(s.PublicHandler(), slog.Default())
	ingest := newHTTPServer(s.IngestHandler(), slog.Default())
	local := newHTTPServer(s.LocalHandler(), slog.Default())

	errc := make(chan error, 4)
	run := func(srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}
	go run(public, publicLn)
	go run(ingest, ingestLn)
	if localLn != nil {
		go run(local, localLn)
	}

	if tailnet != nil {
		go func() {
			defer journal.LogPanic(slog.Default())
			if ln, err := tailnet(ctx); err == nil {
				run(ingest, ln)
			}
		}()
	}

	watching := make(chan struct{})
	go func() {
		defer close(watching)
		defer journal.LogPanic(slog.Default())
		s.WatchSilence(ctx)
	}()
	slog.Info("serving", "page", publicLn.Addr(), "reports", ingestLn.Addr(), "hosts", len(s.hosts))

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
	_ = local.Shutdown(shutdownCtx)

	// An alert already decided must not die with the process, and one being
	// decided must finish deciding before the queue is drained.
	<-watching
	s.Flush()
	return err
}

// newHTTPServer bounds every phase of a request. The public side is reached
// from the internet through Caddy, and the ingest side by anything on the
// tailnet, so neither may let a slow client hold a connection open.
//
// net/http's own errors go to log at error level. Both sides are plain HTTP
// on a bound address, so there is no TLS handshake noise to flood it: what
// arrives is a bug of ours, such as a handler panic, or a failing accept.
func newHTTPServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
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
				slog.Info("bound; reports from other hosts can arrive", "addr", addr)
			}
			return ln, nil
		}
		if !failing {
			slog.Warn("cannot bind, retrying", "addr", addr, "every", every, "err", err)
			failing = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}
