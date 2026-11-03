package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// stateVersion is the state.json schema; a file from another schema is set aside.
const stateVersion = 1

// BundleState caches one bundle's resolved ids in one space: the root
// (the type or collection id), xKey → propId, part key → storage
// collection, and the root's dataview object.
type BundleState struct {
	RootId      string            `json:"rootId"`
	Props       map[string]string `json:"props,omitempty"`
	Collections map[string]string `json:"collections,omitempty"`
	Dataview    string            `json:"dataview,omitempty"`
}

// Space is one space this machine works in: the private one (memories,
// config, sessions, machine) or a collab space shared with other
// accounts (collabs, shared docs, tasks, plans, converged project pages).
type Space struct {
	Id       string          `json:"id"`
	Name     string          `json:"name"`
	Kind     string          `json:"kind"` // private | collab
	Usecases map[string]bool `json:"usecases,omitempty"`
	// Bundles keyed by short name; "collab" is absent on a server that
	// still reserves the chat module.
	Bundles         map[string]*BundleState `json:"bundles,omitempty"`
	MachineObjectId string                  `json:"machineObjectId,omitempty"`
	// SetupVersion gates the fast path in ensureSpaceSetup: bumping the
	// const forces one full re-ensure on every machine.
	SetupVersion int    `json:"setupVersion,omitempty"`
	Created      bool   `json:"-"` // created by this run: no registry to wait for
	st           *State `json:"-"`
}

const (
	kindPrivate = "private"
	kindCollab  = "collab"
)

// State is the per-machine bootstrap cache (~/.any-agents/state.json).
type State struct {
	Version       int               `json:"version"`
	Identity      string            `json:"identity,omitempty"` // own account id
	TechSpaceId   string            `json:"techSpaceId,omitempty"`
	Private       string            `json:"private,omitempty"` // private space id
	Spaces        map[string]*Space `json:"spaces,omitempty"`
	LastSessionId string            `json:"lastSessionId,omitempty"`
}

// stateDir is ~/.any-agents, or $ANY_AGENTS_HOME when set (tests keep
// their state apart).
func stateDir() string {
	if h := os.Getenv("ANY_AGENTS_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".any-agents")
}

func statePath() string { return filepath.Join(stateDir(), "state.json") }

// loadState reads the cache. A file from another schema is set aside
// as state.v<N>.json so the current one starts from an empty state.
func loadState() (*State, error) {
	st := &State{}
	b, err := os.ReadFile(statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	if st.Version < stateVersion {
		_ = os.Rename(statePath(), filepath.Join(stateDir(), fmt.Sprintf("state.v%d.json", max(st.Version, 1))))
		return &State{}, nil
	}
	for _, sp := range st.Spaces {
		sp.st = st
	}
	return st, nil
}

func (st *State) save() error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	st.Version = stateVersion
	b, _ := json.MarshalIndent(st, "", "  ")
	return writePrivateFile(statePath(), b)
}

// space returns the cached space by id, or nil.
func (st *State) space(id string) *Space {
	if st.Spaces == nil {
		return nil
	}
	return st.Spaces[id]
}

// addSpace registers a space in the cache (idempotent).
func (st *State) addSpace(id, name, kind string) *Space {
	if st.Spaces == nil {
		st.Spaces = map[string]*Space{}
	}
	sp := st.Spaces[id]
	if sp == nil {
		sp = &Space{Id: id, Bundles: map[string]*BundleState{}, Usecases: map[string]bool{}}
		st.Spaces[id] = sp
	}
	sp.Name, sp.Kind, sp.st = name, kind, st
	return sp
}

// private is the private space (nil before install).
func (st *State) private() *Space { return st.space(st.Private) }

// collabs lists the collab spaces by name.
func (st *State) collabs() []*Space {
	var out []*Space
	for _, sp := range st.Spaces {
		if sp.Kind == kindCollab {
			out = append(out, sp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// pick resolves a --space argument: empty is the private space; otherwise
// a space id, an any:// link, or a (case-insensitive) space name.
func (st *State) pick(flag string) (*Space, error) {
	if flag == "" {
		if sp := st.private(); sp != nil {
			return sp, nil
		}
		return nil, fmt.Errorf("not installed — run `any-agents install`")
	}
	if sp := st.space(parseSpaceId(flag)); sp != nil {
		return sp, nil
	}
	if sp := st.private(); sp != nil && strings.EqualFold(sp.Name, flag) {
		return sp, nil
	}
	var found []*Space
	for _, sp := range st.collabs() {
		if strings.EqualFold(sp.Name, flag) {
			found = append(found, sp)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("space %q is not set up here — `any-agents space add %s`", flag, flag)
	case 1:
		return found[0], nil
	}
	return nil, fmt.Errorf("%d collab spaces are named %q — pass the space id", len(found), flag)
}

// parseSpaceId accepts a space id or an any://o/<space>/<id> link.
func parseSpaceId(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "any://o/") {
		rest := strings.TrimPrefix(s, "any://o/")
		if i := strings.Index(rest, "/"); i > 0 {
			return rest[:i]
		}
	}
	return s
}

// bundle returns the cached state for a bundle short name (never nil).
func (sp *Space) bundle(name string) *BundleState {
	if sp.Bundles == nil {
		sp.Bundles = map[string]*BundleState{}
	}
	b := sp.Bundles[name]
	if b == nil {
		b = &BundleState{Props: map[string]string{}, Collections: map[string]string{}}
		sp.Bundles[name] = b
	}
	return b
}

// lookup reads a bundle's cached state without creating it.
func (sp *Space) lookup(name string) *BundleState {
	if b := sp.Bundles[name]; b != nil {
		return b
	}
	return &BundleState{}
}

// owner is a bundle's root: the type id, or the collection id for the
// collection bundles — the id property values are grouped under.
func (sp *Space) owner(name string) string { return sp.lookup(name).RootId }

// has reports whether a bundle resolved in this space (collab is optional).
func (sp *Space) has(name string) bool { return sp.owner(name) != "" }

// isA reports whether an object is of a bundle: filed under it for a
// collection bundle, its type for a type bundle.
func (sp *Space) isA(row map[string]any, name string) bool {
	if collectionBundles[name] {
		return inCollection(row, sp.owner(name))
	}
	return typeOf(row) == sp.owner(name)
}
func (sp *Space) prop(name, xkey string) string { return sp.bundle(name).Props[xkey] }
func (sp *Space) coll(key string) string        { return sp.bundle("app").Collections[key] }
func (sp *Space) save() error                   { return sp.st.save() }

// ready reports whether every required bundle resolved in the space.
func (sp *Space) ready() bool {
	if sp.Id == "" || !sp.Usecases["collections"] {
		return false
	}
	for _, n := range bundleNames {
		if !optionalBundles[n] && !sp.has(n) {
			return false
		}
	}
	return true
}

// me is the own identity as a member link (the value of member properties).
func (st *State) me() string { return identityLink(st.Identity) }

func identityLink(identity string) string { return "any://u/" + identity }

// SyncEntry tracks one record's last-synced fingerprint on this machine.
type SyncEntry struct {
	File            string  `json:"file"` // path relative to the sync root
	Hash            string  `json:"hash"` // sha256 of the normalized content at last sync
	RemoteUpdatedAt float64 `json:"remoteUpdatedAt"`
}

type SyncState struct {
	Records map[string]*SyncEntry `json:"records"`
	path    string
}

func loadSyncState(name string) (*SyncState, error) {
	p := filepath.Join(stateDir(), "sync", name+".json")
	ss := &SyncState{Records: map[string]*SyncEntry{}, path: p}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ss, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, ss); err != nil {
		return nil, err
	}
	if ss.Records == nil {
		ss.Records = map[string]*SyncEntry{}
	}
	return ss, nil
}

func (ss *SyncState) save() error {
	if err := os.MkdirAll(filepath.Dir(ss.path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(ss, "", "  ")
	return writePrivateFile(ss.path, b)
}

// Replace as one rename so a concurrent reader sees a complete file.
func writePrivateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".any-agents-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
