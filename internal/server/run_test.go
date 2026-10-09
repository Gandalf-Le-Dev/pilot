package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
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
	publicLn, ingestLn := listenLocal(t), listenLocal(t)

	tailnetStopped := make(chan struct{})
	tailnet := func(ctx context.Context) (net.Listener, error) {
		<-ctx.Done()
		close(tailnetStopped)
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, publicLn, ingestLn, tailnet) }()

	resp, err := http.Get("http://" + publicLn.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok\n" {
		t.Fatalf("healthz = %q", body)
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
		done <- serve(context.Background(), s, brokenListener{listenLocal(t)}, ingestLn, tailnet)
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
