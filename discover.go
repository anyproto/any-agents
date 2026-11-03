package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Server discovery. The Any desktop app and `any run` both record the
// bound address in <dataDir>/server.addr (the data dir root, or one
// account dir under it), so a machine where the app already runs the
// server needs no address: every recorded address whose health answers
// is a running server, and the one serving the account this machine
// set up wins when there are several.

type serverInfo struct {
	Addr    string
	Account string
}

// anyDataRoot is any's data dir: $ANY_DATA_DIR, else ~/.any.
func anyDataRoot() string {
	if v := strings.TrimSpace(os.Getenv("ANY_DATA_DIR")); v != "" {
		if strings.HasPrefix(v, "~/") {
			home, _ := os.UserHomeDir()
			v = filepath.Join(home, v[2:])
		}
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".any")
}

// recordedAddrs reads every server.addr under the data root: the root
// itself and its account dirs.
func recordedAddrs(root string) []string {
	if root == "" {
		return nil
	}
	var dirs []string
	dirs = append(dirs, root)
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(root, e.Name()))
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "server.addr"))
		if err != nil {
			continue
		}
		addr := strings.TrimSpace(string(b))
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, addr)
	}
	return out
}

// probeServer answers the account a server at addr serves, or "" when
// nothing healthy listens there.
func probeServer(addr string, timeout time.Duration) (string, bool) {
	hc := &http.Client{Timeout: timeout}
	resp, err := hc.Get("http://" + addr + "/v1/health")
	if err != nil {
		return "", false
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", false
	}
	resp, err = hc.Get("http://" + addr + "/v1/account")
	if err != nil {
		return "", true
	}
	defer resp.Body.Close()
	var acc struct {
		Id string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&acc)
	return acc.Id, true
}

// discoverServers lists the running servers recorded under root.
func discoverServers(root string, timeout time.Duration) []serverInfo {
	var out []serverInfo
	for _, addr := range recordedAddrs(root) {
		if account, ok := probeServer(addr, timeout); ok {
			out = append(out, serverInfo{Addr: addr, Account: account})
		}
	}
	return out
}

// pickServer chooses among running servers: the one serving identity
// when known, else the only one; "" when none or ambiguous.
func pickServer(found []serverInfo, identity string) string {
	if identity != "" {
		for _, s := range found {
			if s.Account == identity {
				return s.Addr
			}
		}
	}
	if len(found) == 1 {
		return found[0].Addr
	}
	return ""
}

const defaultAddr = "127.0.0.1:2046"

var resolvedAddr string

// resolveAddr is the server address for this process: $ANY_AGENTS_ADDR,
// else the address `install --addr` pinned when it still answers, else
// the running server recorded by any, else the default.
func resolveAddr() string {
	if resolvedAddr != "" {
		return resolvedAddr
	}
	if v := os.Getenv("ANY_AGENTS_ADDR"); v != "" {
		resolvedAddr = v
		return v
	}
	identity := ""
	if st, err := loadState(); err == nil {
		identity = st.Identity
		if st.Addr != "" {
			if _, ok := probeServer(st.Addr, 500*time.Millisecond); ok {
				resolvedAddr = st.Addr
				return st.Addr
			}
		}
	}
	if addr := pickServer(discoverServers(anyDataRoot(), 500*time.Millisecond), identity); addr != "" {
		resolvedAddr = addr
		return addr
	}
	resolvedAddr = defaultAddr
	return defaultAddr
}
