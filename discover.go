package main

import (
	"encoding/json"
	"fmt"
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

// pinPath is the address `install` pinned: a file of its own, written
// only by the installer, so no state round-trip of a hook can overwrite
// it and a pin survives until the next install.
func pinPath() string { return filepath.Join(stateDir(), "server-address") }

func readPin() (string, error) {
	b, err := os.ReadFile(pinPath())
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func writePin(addr string) error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	tmp := pinPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(addr+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, pinPath())
}

func clearPin() error {
	if err := os.Remove(pinPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// chooseAddr is the precedence: the environment, else a pin (which stays
// authoritative whether or not the server answers, so an outage never
// redirects a machine to another account's server), else the one
// running server discovery can attribute, else the default. found is
// called only when nothing is pinned.
func chooseAddr(env, pin, legacyPin string, found func() []serverInfo, identity string) (addr, source string) {
	switch {
	case env != "":
		return env, "ANY_AGENTS_ADDR"
	case pin != "":
		return pin, "pinned by install"
	case legacyPin != "":
		return legacyPin, "pinned by install"
	}
	if addr := pickServer(found(), identity); addr != "" {
		return addr, "discovered"
	}
	return defaultAddr, "default"
}

var resolvedAddr, resolvedAddrSource string

// resolveAddr is the server address for this process, cached. A pin
// file that cannot be read is fatal: guessing another server is worse
// than stopping.
func resolveAddr() string {
	if resolvedAddr != "" {
		return resolvedAddr
	}
	pin, err := readPin()
	if err != nil {
		fatal(fmt.Errorf("pinned server address: %w", err))
	}
	identity, legacy := "", ""
	if st, err := loadState(); err == nil {
		identity, legacy = st.Identity, st.Addr
	} else {
		fmt.Fprintf(os.Stderr, "any-agents: state: %v\n", err)
	}
	resolvedAddr, resolvedAddrSource = chooseAddr(os.Getenv("ANY_AGENTS_ADDR"), pin, legacy,
		func() []serverInfo { return discoverServers(anyDataRoot(), 500*time.Millisecond) }, identity)
	return resolvedAddr
}
