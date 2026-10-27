package link

import "testing"

func TestRedactNames(t *testing.T) {
	names := []string{"web", "my-web-app", "agent"}
	for in, want := range map[string]string{
		"web: updated":                    "agent-1: updated",
		"agent my-web-app: removed":       "agent-3 agent-2: removed",
		"p3000-web.view.example.org":      "p3000-agent-1.view.example.org",
		"web web, web":                    "agent-1 agent-1, agent-1",
		"webhook and cobweb stay":         "webhook and cobweb stay",
		"agent agent: ok":                 "agent-3 agent-3: ok",
		"agent-1 is an alias, not a name": "agent-3-1 is an alias, not a name",
	} {
		if got := redactNames(in, names); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
