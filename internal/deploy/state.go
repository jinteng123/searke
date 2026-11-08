package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Stage names recorded per node. Stages are monotonic: apply resumes
// from the first incomplete stage.
const (
	StagePreflight   = "preflight"
	StageDistributed = "distributed"
	StageConfigured  = "configured"
	StageInstalled   = "installed"
	StageRunning     = "running"
	StageReady       = "ready"
)

var stageOrder = []string{
	StagePreflight, StageDistributed, StageConfigured,
	StageInstalled, StageRunning, StageReady,
}

func stageIndex(s string) int {
	for i, v := range stageOrder {
		if v == s {
			return i
		}
	}
	return -1
}

// NodeState records per-node progress.
type NodeState struct {
	Role    string    `json:"role"`
	Arch    string    `json:"arch"`
	Stage   string    `json:"stage"`
	Updated time.Time `json:"updated"`
	Detail  string    `json:"detail,omitempty"`
}

// HistoryEntry is one apply/plan action log line.
type HistoryEntry struct {
	TS     time.Time `json:"ts"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
}

// State is the local apply state file. It contains no secrets: the
// node token is never persisted here.
type State struct {
	Cluster     string               `json:"cluster"`
	Bundle      string               `json:"bundle"`
	Version     string               `json:"version"`
	CNI         string               `json:"cni"`
	FirstServer string               `json:"firstServer"`
	Nodes       map[string]NodeState `json:"nodes"`
	History     []HistoryEntry       `json:"history"`
	path        string
}

// StatePath returns the conventional state file location for a cluster.
func StatePath(dir, clusterName string) string {
	return filepath.Join(dir, ".searke", "state-"+clusterName+".json")
}

// LoadState reads the state file or returns a fresh state.
func LoadState(path, clusterName, bundleRef, version, cni string) (*State, error) {
	s := &State{
		Cluster: clusterName,
		Bundle:  bundleRef,
		Version: version,
		CNI:     cni,
		Nodes:   map[string]NodeState{},
		path:    path,
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	s.path = path
	if s.Nodes == nil {
		s.Nodes = map[string]NodeState{}
	}
	// Guard against a stale state file being reused across bundles.
	if s.Version != "" && version != "" && s.Version != version {
		return nil, fmt.Errorf("state %s was recorded for RKE2 %s but bundle has %s; use `searke upgrade` for version changes, or use the matching bundle",
			path, s.Version, version)
	}
	return s, nil
}

// LoadStateUnchecked loads the state file without the version guard —
// used by upgrade where the bundle version deliberately differs.
func LoadStateUnchecked(path, clusterName string) (*State, error) {
	return LoadState(path, clusterName, "", "", "")
}

// Save persists the state file with owner-only permissions via temp
// file + rename, so a crash mid-write never corrupts resume progress.
func (s *State) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Mark records a node's completed stage and saves. Stages are
// monotonic: marking an earlier stage than the recorded one is a no-op,
// so an interrupted apply never loses resume progress.
func (s *State) Mark(node, role, arch, stage, detail string) error {
	if cur, ok := s.Nodes[node]; ok && stageIndex(cur.Stage) > stageIndex(stage) {
		return nil
	}
	s.Nodes[node] = NodeState{Role: role, Arch: arch, Stage: stage, Updated: time.Now().UTC(), Detail: detail}
	return s.Save()
}

// Done reports whether the node completed the stage.
func (s *State) Done(node, stage string) bool {
	ns, ok := s.Nodes[node]
	return ok && stageIndex(ns.Stage) >= stageIndex(stage)
}

// Node returns a node's recorded state.
func (s *State) Node(name string) (NodeState, bool) {
	ns, ok := s.Nodes[name]
	return ns, ok
}

// Log appends a history entry.
func (s *State) Log(action, detail string) error {
	s.History = append(s.History, HistoryEntry{TS: time.Now().UTC(), Action: action, Detail: detail})
	return s.Save()
}

// RemoveNode drops a node's recorded state (used by `searke delete`
// after the node has left the cluster).
func (s *State) RemoveNode(name string) {
	delete(s.Nodes, name)
}

// Reset deletes the state file entirely — used by uninstall so a fresh
// apply starts from a clean slate.
func (s *State) Reset() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	s.Nodes = map[string]NodeState{}
	s.History = nil
	return nil
}
