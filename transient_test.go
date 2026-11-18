package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

func TestTransient(t *testing.T) {
	yes := []error{
		io.EOF,
		syscall.ECONNREFUSED,
		&apiError{Status: 401, Code: "auth.required"},
		&apiError{Status: 503},
	}
	no := []error{
		nil,
		errors.New("boom"),
		&apiError{Status: 401, Code: "auth.invalid"},
		&apiError{Status: 404, Code: "not_found"},
		&apiError{Status: 403},
	}
	for _, e := range yes {
		if !transient(e) {
			t.Errorf("%v should be transient", e)
		}
	}
	for _, e := range no {
		if transient(e) {
			t.Errorf("%v should not be transient", e)
		}
	}
}

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("ANY_AGENTS_HOME", t.TempDir())
	return &Client{base: srv.URL, hc: srv.Client()}
}

// A body cut short by a restarting server is a transport error.
func TestCutBodyIsTransient(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x`))
	})
	if _, err := c.Account(); !transient(err) {
		t.Fatalf("cut body: %v should be transient", err)
	}
}

func TestWaitOutlastsOutageThenReportsServerDown(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"auth.required","message":"no account authorized"}}`))
	})
	old := maxOutage
	maxOutage = 3 * time.Second
	defer func() { maxOutage = old }()
	start := time.Now()
	_, _, err := collabWait(c, &State{}, &Space{Id: "sp"}, "obj", time.Hour, false)
	var down *serverDownError
	if !errors.As(err, &down) {
		t.Fatalf("want serverDownError, got %v", err)
	}
	if d := time.Since(start); d < 2*time.Second || d > 6*time.Second {
		t.Fatalf("waited %v, want about the 3s outage limit", d)
	}
}

func TestWaitFailsFastOnPermanentError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no"}}`))
	})
	start := time.Now()
	_, _, err := collabWait(c, &State{}, &Space{Id: "sp"}, "obj", time.Minute, false)
	var down *serverDownError
	if err == nil || errors.As(err, &down) || time.Since(start) > 3*time.Second {
		t.Fatalf("want a prompt permanent error, got %v after %v", err, time.Since(start))
	}
}

// A real refused connection (the app is down) is retried, then reported.
func TestRefusedConnectionIsRetried(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(http.NotFoundHandler())
	c.base = srv.URL
	srv.Close() // nothing listens there now
	if _, err := c.Account(); !transient(err) {
		t.Fatalf("refused: %v should be transient", err)
	}
	old := maxOutage
	maxOutage = 3 * time.Second
	defer func() { maxOutage = old }()
	start := time.Now()
	_, _, err := collabWait(c, &State{}, &Space{Id: "sp"}, "obj", time.Hour, false)
	var down *serverDownError
	if !errors.As(err, &down) || time.Since(start) < 2*time.Second {
		t.Fatalf("want serverDownError after ~3s, got %v after %v", err, time.Since(start))
	}
}

// The attempt after the last sleep still runs: a server back right at the
// end of the outage budget is not reported down.
func TestWaitLastAttemptRuns(t *testing.T) {
	back := time.Now().Add(2500 * time.Millisecond)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if time.Now().Before(back) {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"code":"server.unavailable","message":"x"}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	old := maxOutage
	maxOutage = 3 * time.Second
	defer func() { maxOutage = old }()
	_, _, err := collabWait(c, &State{}, &Space{Id: "sp"}, "obj", 3200*time.Millisecond, false)
	var down *serverDownError
	if errors.As(err, &down) {
		t.Fatalf("server was back before the budget ended, got %v", err)
	}
}

// No cursor yet and the server down at the start: no empty cursor is
// recorded, so the backlog does not wake the agent after the restart.
func TestWaitNoCursorWhileDown(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"code":"server.unavailable","message":"x"}}`))
	})
	old := maxOutage
	maxOutage = time.Second
	defer func() { maxOutage = old }()
	_, _, _ = collabWait(c, &State{}, &Space{Id: "sp"}, "obj-nocursor", time.Hour, false)
	if _, ok := loadCursor("obj-nocursor", currentSessionId()); ok {
		t.Fatal("a cursor was saved while the server was down")
	}
}
