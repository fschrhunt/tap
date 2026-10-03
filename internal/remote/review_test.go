package remote

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fschrhunt/tap/internal/config"
)

// TestInitializationWaitersCancelIndependently protects shared startup from any caller's cancellation.
func TestInitializationWaitersCancelIndependently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	backend, manager, cleanup := testHandler(t, path)
	defer cleanup()
	entered, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	host := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && requests.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		backend.ServeHTTP(w, r)
	}))
	host.TLS = &tls.Config{Certificates: []tls.Certificate{manager.certificate()}, MinVersion: tls.VersionTLS12}
	host.StartTLS()
	defer host.Close()
	paired, err := Pair(context.Background(), host.URL, manager.fingerprint, manager.execCode, "test client", "execution")
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(config.Remote{URL: host.URL, PeerID: paired.ID, Fingerprint: manager.fingerprint, Token: paired.Token}, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() { _, err := c.Listing(first, true); firstDone <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("initialization did not start")
	}
	second, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	secondDone := make(chan error, 1)
	go func() { _, err := c.Listing(second, true); secondDone <- err }()
	cancelSecond()
	cancelFirst()
	for _, done := range []chan error{firstDone, secondDone} {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled waiter: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancellation blocked behind initialization")
		}
	}
	close(release)
	if _, err := c.Listing(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// Initialize, initialized notification, and listing: no second initialization.
	if requests.Load() != 3 {
		t.Fatalf("shared initialization restarted: %d POSTs", requests.Load())
	}
}

// TestCloseCancelsAdministration prevents a stalled HTTP response outliving its relay client.
func TestCloseCancelsAdministration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	manager, err := newDeviceManager(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	backend, cleanup, err := handler(path, "test", manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	entered := make(chan struct{})
	host := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/servers" {
			_, _ = io.Copy(io.Discard, r.Body)
			close(entered)
			<-r.Context().Done()
			return
		}
		backend.ServeHTTP(w, r)
	}))
	host.TLS = &tls.Config{Certificates: []tls.Certificate{manager.certificate()}, MinVersion: tls.VersionTLS12}
	host.StartTLS()
	defer host.Close()
	paired, err := Pair(context.Background(), host.URL, manager.fingerprint, manager.adminCode, "test client", "admin")
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(config.Remote{URL: host.URL, PeerID: paired.ID, Fingerprint: manager.fingerprint, Token: paired.Token}, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan error, 1)
	go func() { _, err := c.Edit(context.Background(), "test", nil); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("administration did not start")
	}
	c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled administration succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("client close did not cancel administration")
	}
}

// roundTripFunc lets tests inspect request deadlines without a slow external peer.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip supplies the injected test response.
func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestAdministrationHasDeadline protects CLI edits whose caller has no deadline.
func TestAdministrationHasDeadline(t *testing.T) {
	server, manager := testHost(t, filepath.Join(t.TempDir(), "servers.json"))
	c := pairedTestClient(t, server, manager, "admin")
	var err error
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Error("administration has no bounded deadline")
		}
		return nil, errors.New("synthetic refusal")
	})
	_, _ = c.Edit(context.Background(), "test", nil)
}
