package deploy

import (
	"strings"
	"testing"

	"github.com/jint/searke/internal/cluster"
)

func opsTestCluster() *cluster.Cluster {
	return &cluster.Cluster{
		Spec: cluster.Spec{
			Servers: []cluster.Node{
				{Name: "m1", Address: "10.0.0.1"},
				{Name: "m2", Address: "10.0.0.2"},
				{Name: "m3", Address: "10.0.0.3"},
			},
			Agents: []cluster.Node{
				{Name: "w1", Address: "10.0.1.1"},
			},
		},
	}
}

func TestSelectNodes(t *testing.T) {
	c := opsTestCluster()

	all, err := selectNodes(c, nil)
	if err != nil || len(all) != 4 {
		t.Fatalf("empty selector: got %d nodes, err %v", len(all), err)
	}

	// by name and by address, deduplicated
	got, err := selectNodes(c, []string{"m2", "10.0.1.1", "m2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "m2" || got[1].Name != "w1" {
		t.Fatalf("got %+v", got)
	}

	if _, err := selectNodes(c, []string{"nope"}); err == nil ||
		!strings.Contains(err.Error(), "not in the cluster") {
		t.Fatalf("unknown node should error, got %v", err)
	}
}

func TestParseNodeSelectors(t *testing.T) {
	got := ParseNodeSelectors(" m1 ,10.0.0.2,, w1 ")
	want := []string{"m1", "10.0.0.2", "w1"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if got := ParseNodeSelectors(""); len(got) != 0 {
		t.Fatalf("empty csv: got %v", got)
	}
}

func TestReplaceHostsBlock(t *testing.T) {
	block := hostsBlockBegin + "\n10.0.0.1 m1\n" + hostsBlockEnd + "\n"

	// no existing block: appended after existing content
	out := replaceHostsBlock("127.0.0.1 localhost\n", block)
	if !strings.Contains(out, "127.0.0.1 localhost") || !strings.HasSuffix(out, block) {
		t.Fatalf("append: %q", out)
	}

	// existing block replaced in place, other lines kept
	cur := "127.0.0.1 localhost\n# searke-hosts-begin\n10.9.9.9 stale\n# searke-hosts-end\n::1 localhost\n"
	out = replaceHostsBlock(cur, block)
	if strings.Contains(out, "stale") || !strings.Contains(out, "::1 localhost") ||
		!strings.HasSuffix(out, block) {
		t.Fatalf("replace: %q", out)
	}
	// exactly one managed block
	if strings.Count(out, hostsBlockBegin) != 1 {
		t.Fatalf("duplicate markers: %q", out)
	}
}
