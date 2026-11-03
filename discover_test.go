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

func TestChooseAddr(t *testing.T) {
	live := []serverInfo{{Addr: "127.0.0.1:1111", Account: "A1"}, {Addr: "127.0.0.1:2222", Account: "B2"}}
	noDiscovery := func() []serverInfo { t.Fatal("discovery ran although an address was pinned"); return nil }
	cases := []struct {
		name, env, pin, legacy string
		found                  func() []serverInfo
		identity               string
		want, source           string
	}{
		{"env over pin and discovery", "127.0.0.1:9", "127.0.0.1:8", "", noDiscovery, "A1", "127.0.0.1:9", "ANY_AGENTS_ADDR"},
		{"pin stays authoritative, no discovery", "", "127.0.0.1:8", "", noDiscovery, "A1", "127.0.0.1:8", "pinned by install"},
		{"legacy state pin counts", "", "", "127.0.0.1:7", noDiscovery, "A1", "127.0.0.1:7", "pinned by install"},
		{"unpinned: the account's server", "", "", "", func() []serverInfo { return live }, "B2", "127.0.0.1:2222", "discovered"},
		{"unpinned: a single server", "", "", "", func() []serverInfo { return live[:1] }, "", "127.0.0.1:1111", "discovered"},
		{"unpinned: ambiguous falls to the default", "", "", "", func() []serverInfo { return live }, "", defaultAddr, "default"},
		{"unpinned: nothing running", "", "", "", func() []serverInfo { return nil }, "A1", defaultAddr, "default"},
	}
	for _, c := range cases {
		got, source := chooseAddr(c.env, c.pin, c.legacy, c.found, c.identity)
		if got != c.want || source != c.source {
			t.Errorf("%s: got %s (%s), want %s (%s)", c.name, got, source, c.want, c.source)
		}
	}
}

func TestPinFile(t *testing.T) {
	t.Setenv("ANY_AGENTS_HOME", t.TempDir())
	if pin, err := readPin(); err != nil || pin != "" {
		t.Fatalf("fresh home: pin %q err %v", pin, err)
	}
	if err := writePin("127.0.0.1:4242"); err != nil {
		t.Fatal(err)
	}
	if pin, err := readPin(); err != nil || pin != "127.0.0.1:4242" {
		t.Fatalf("after write: pin %q err %v", pin, err)
	}
	if err := clearPin(); err != nil {
		t.Fatal(err)
	}
	if err := clearPin(); err != nil {
		t.Fatalf("clearing twice: %v", err)
	}
	if pin, _ := readPin(); pin != "" {
		t.Fatalf("after clear: pin %q", pin)
	}
	// an unreadable pin surfaces instead of being guessed around
	if err := os.Mkdir(pinPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readPin(); err == nil {
		t.Fatal("a pin that cannot be read returned no error")
	}
}
