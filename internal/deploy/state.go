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
	Cluster     string              `json:"cluster"`
	Bundle      string              `json:"bundle"`
	Version     string              `json:"version"`
	CNI         string              `json:"cni"`
	FirstServer string              `json:"firstServer"`
	Nodes       map[string]NodeState `json:"nodes"`
	History     []HistoryEntry      `json:"history"`
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
		return nil, fmt.Errorf("state %s was recorded for RKE2 %s but bundle has %s; remove the state file or use the matching bundle",
			path, s.Version, version)
	}
	return s, nil
}

// Save persists the state file with owner-only permissions.
func (s *State) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

// Mark records a node's completed stage and saves.
func (s *State) Mark(node, role, arch, stage, detail string) error {
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
