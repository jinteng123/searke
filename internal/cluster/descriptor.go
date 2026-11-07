package cluster

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// UpdateDescriptor appends nodes to spec.servers/spec.agents in an
// existing cluster.yaml and optionally updates spec.ssh fields. The
// document is edited at the yaml.Node level so comments survive, but
// the whole file is re-encoded: whitespace normalises to two-space
// indent and flow mappings may reflow (e.g. "{ name: x }" becomes
// "{name: x}"). The write is atomic via a temp file + rename.
func UpdateDescriptor(path string, servers, agents []Node, ssh *SSHSpec) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: expected a mapping document", path)
	}
	root := doc.Content[0]
	spec := mapValue(root, "spec")
	if spec == nil || spec.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: no spec section found", path)
	}
	if len(servers) > 0 {
		seq := ensureSeq(spec, "servers")
		for _, n := range servers {
			seq.Content = append(seq.Content, nodeEntry(n))
		}
	}
	if len(agents) > 0 {
		seq := ensureSeq(spec, "agents")
		for _, n := range agents {
			seq.Content = append(seq.Content, nodeEntry(n))
		}
	}
	if ssh != nil {
		sm := mapValue(spec, "ssh")
		if sm == nil || sm.Kind != yaml.MappingNode {
			sm = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setMapValue(spec, "ssh", sm)
		}
		if ssh.User != "" {
			setMapValue(sm, "user", strNode(ssh.User))
		}
		if ssh.IdentityFile != "" {
			setMapValue(sm, "identityFile", strNode(ssh.IdentityFile))
		}
		if ssh.Port != 0 {
			setMapValue(sm, "port", intNode(ssh.Port))
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	// Atomic replace: never leave a half-written descriptor.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cluster-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// RemoveFromDescriptor deletes server/agent entries whose name is in
// names. Same atomic temp+rename write as UpdateDescriptor; comments
// outside the lists are preserved.
func RemoveFromDescriptor(path string, names []string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: expected a mapping document", path)
	}
	spec := mapValue(doc.Content[0], "spec")
	if spec == nil || spec.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: no spec section found", path)
	}
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	for _, key := range []string{"servers", "agents"} {
		seq := mapValue(spec, key)
		if seq == nil || seq.Kind != yaml.SequenceNode {
			continue
		}
		var kept []*yaml.Node
		for _, entry := range seq.Content {
			nv := mapValue(entry, "name")
			if nv != nil && drop[nv.Value] {
				continue
			}
			kept = append(kept, entry)
		}
		seq.Content = kept
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cluster-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func setMapValue(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, strNode(key), val)
}

func ensureSeq(spec *yaml.Node, key string) *yaml.Node {
	seq := mapValue(spec, key)
	if seq == nil || seq.Kind != yaml.SequenceNode {
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setMapValue(spec, key, seq)
	}
	return seq
}

func nodeEntry(n Node) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle,
		Content: []*yaml.Node{strNode("name"), strNode(n.Name), strNode("address"), strNode(n.Address)},
	}
}

func strNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func intNode(i int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(i)}
}
