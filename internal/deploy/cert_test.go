package deploy

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMergeSANLists(t *testing.T) {
	got := mergeSANLists([]string{"b.example.com", "10.0.0.1 "}, []string{"10.0.0.1", "a.example.com"})
	want := []string{"10.0.0.1", "a.example.com", "b.example.com"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := mergeSANLists(nil, nil); len(got) != 0 {
		t.Fatalf("empty: got %v", got)
	}
}

// writeRecorder captures WriteFile payloads.
type writeRecorder struct {
	fakeExecutor
	data []byte
	path string
	mode os.FileMode
}

func (w *writeRecorder) WriteFile(p string, b []byte, m os.FileMode) error {
	w.path, w.data, w.mode = p, b, m
	return nil
}

func TestMergeRemoteSans(t *testing.T) {
	existing := "node-name: m1\ncni: canal\ntls-san:\n  - 10.0.0.1\n"
	rec := &writeRecorder{fakeExecutor: fakeExecutor{
		outs: map[string]string{"cat " + remoteConfigPath: existing},
	}}
	if err := mergeRemoteSans(rec, []string{"vip.example.com", "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if rec.path != remoteConfigPath || rec.mode != 0o600 {
		t.Fatalf("wrote %s mode %o", rec.path, rec.mode)
	}
	var m map[string]any
	if err := yaml.Unmarshal(rec.data, &m); err != nil {
		t.Fatalf("written yaml invalid: %v\n%s", err, rec.data)
	}
	if m["node-name"] != "m1" || m["cni"] != "canal" {
		t.Fatalf("existing keys lost: %v", m)
	}
	sans := toStringSlice(m["tls-san"])
	if len(sans) != 2 || !strings.Contains(strings.Join(sans, ","), "vip.example.com") {
		t.Fatalf("tls-san merge wrong: %v", sans)
	}
}
