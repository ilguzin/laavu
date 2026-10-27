package priv

import (
	"strings"
	"testing"

	"github.com/ilguzin/laavu/internal/config"
)

func TestNftRules(t *testing.T) {
	r := NftRules([]config.Agent{
		{Name: "a", UID: 1001, Ports: [2]int{20000, 20999}},
		{Name: "b", UID: 1002, Ports: [2]int{21000, 21999}},
		{Name: "c", UID: 1003, Ports: [2]int{22000, 22999}},
	})
	for _, want := range []string{
		`tcp dport 20000-20999 meta skuid { 1002, 1003 } reject`,
		`tcp dport 21000-21999 meta skuid { 1001, 1003 } reject`,
		"delete table inet laavu",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("missing %q in\n%s", want, r)
		}
	}
	if strings.Contains(NftRules([]config.Agent{{Name: "a", UID: 1001, Ports: [2]int{20000, 20999}}}), "dport") {
		t.Error("a single agent must have no rules")
	}
	if p := nextPorts([]config.Agent{{Ports: [2]int{20000, 20999}}, {Ports: [2]int{22000, 22999}}}); p != [2]int{21000, 21999} {
		t.Error("free block:", p)
	}
}

func TestReorder(t *testing.T) {
	got := strings.Join(reorder([]string{"demo", "--memory", "1G", "--install-claude"}), " ")
	if got != "--memory 1G --install-claude demo" {
		t.Fatal(got)
	}
}
