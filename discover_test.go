package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeServer(t *testing.T, account string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"ok"}`)) })
	mux.HandleFunc("/v1/account", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"id":"` + account + `"}`)) })
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestDiscoverServers(t *testing.T) {
	root := t.TempDir()
	a := fakeServer(t, "A1")
	b := fakeServer(t, "B2")
	addrA := strings.TrimPrefix(a.URL, "http://")
	addrB := strings.TrimPrefix(b.URL, "http://")
	// root layout + two account dirs, one of them stale (nothing listens)
	os.WriteFile(filepath.Join(root, "server.addr"), []byte(addrA+"\n"), 0o600)
	os.MkdirAll(filepath.Join(root, "B2"), 0o755)
	os.WriteFile(filepath.Join(root, "B2", "server.addr"), []byte(addrB), 0o600)
	os.MkdirAll(filepath.Join(root, "C3"), 0o755)
	os.WriteFile(filepath.Join(root, "C3", "server.addr"), []byte("127.0.0.1:1"), 0o600)

	found := discoverServers(root, 300*time.Millisecond)
	if len(found) != 2 {
		t.Fatalf("discovered %v, want the two live servers", found)
	}
	if got := pickServer(found, "B2"); got != addrB {
		t.Errorf("pick for B2 = %q, want %q", got, addrB)
	}
	if got := pickServer(found, ""); got != "" {
		t.Errorf("pick with two servers and no identity = %q, want ambiguity", got)
	}
	if got := pickServer(found[:1], ""); got != addrA {
		t.Errorf("pick of a single server = %q, want %q", got, addrA)
	}
	if got := pickServer(found, "nobody"); got != "" {
		t.Errorf("pick for an unknown identity with two servers = %q, want \"\"", got)
	}
	if got := discoverServers(filepath.Join(root, "missing"), 100*time.Millisecond); len(got) != 0 {
		t.Errorf("a missing root discovered %v", got)
	}
}
