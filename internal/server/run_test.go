package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// brokenListener fails its first Accept, as a listener torn away underneath
// the server would.
type brokenListener struct{ net.Listener }

func (brokenListener) Accept() (net.Conn, error) { return nil, errors.New("listener gone") }

func TestServeStopsWhenCancelled(t *testing.T) {
	s, _, _ := testServer(t)
	publicLn, ingestLn, localLn := listenLocal(t), listenLocal(t), listenLocal(t)

	tailnetStopped := make(chan struct{})
	tailnet := func(ctx context.Context) (net.Listener, error) {
		<-ctx.Done()
		close(tailnetStopped)
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, publicLn, ingestLn, localLn, tailnet) }()

	resp, err := http.Get("http://" + publicLn.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok\n" {
		t.Fatalf("healthz = %q", body)
	}

	resp, err = http.Get("http://" + localLn.Addr().String() + HostsPath)
	if err != nil {
		t.Fatal(err)
	}
	var view HostsView
	err = json.NewDecoder(resp.Body).Decode(&view)
	resp.Body.Close()
	if err != nil || len(view.Hosts) != 2 {
		t.Fatalf("host view = %+v, %v; want both hosts", view, err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a cancelled server returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancellation")
	}
	select {
	case <-tailnetStopped:
	case <-time.After(time.Second):
		t.Error("the tailnet retry outlived the server")
	}
}

// The host view is for doctor alone; without its listener the page and
// ingest still serve.
func TestServeWithoutTheHostView(t *testing.T) {
	s, _, _ := testServer(t)
	publicLn := listenLocal(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, publicLn, listenLocal(t), nil, nil) }()

	resp, err := http.Get("http://" + publicLn.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d without the host view", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancellation")
	}
}

// A listener failing ends serve with its error, and stops everything else it
// started on the way out rather than leaving it running into the shutdown.
func TestServeReturnsAListenerFailure(t *testing.T) {
	s, _, _ := testServer(t)
	ingestLn := listenLocal(t)

	tailnetStopped := make(chan struct{})
	tailnet := func(ctx context.Context) (net.Listener, error) {
		<-ctx.Done()
		close(tailnetStopped)
		return nil, ctx.Err()
	}

	done := make(chan error, 1)
	go func() {
		done <- serve(context.Background(), s, brokenListener{listenLocal(t)}, ingestLn, listenLocal(t), tailnet)
	}()

	select {
	case err := <-done:
		if err == nil || err.Error() != "listener gone" {
			t.Errorf("err = %v, want the listener's failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve kept running after a listener failed")
	}
	select {
	case <-tailnetStopped:
	case <-time.After(time.Second):
		t.Error("the tailnet retry outlived the failure")
	}
}

// net/http reports a recovered handler panic through ErrorLog, which would
// otherwise reach the journal at info.
func TestHTTPServerErrorsLogAtError(t *testing.T) {
	var buf bytes.Buffer
	srv := newHTTPServer(http.NotFoundHandler(), slog.New(slog.NewTextHandler(&buf, nil)))
	srv.ErrorLog.Print("http: panic serving 127.0.0.1:1: boom")

	if out := buf.String(); !strings.Contains(out, `level=ERROR msg="http: panic serving 127.0.0.1:1: boom"`) {
		t.Errorf("got %q, want it at error level", out)
	}
}
