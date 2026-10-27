package hook

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolTarget(t *testing.T) {
	cases := []struct {
		tool, input, want string
	}{
		{"Edit", `{"file_path":"/home/a/proj/src/x.go","old_string":"secret"}`, "src/x.go"},
		{"Write", `{"file_path":"/etc/hosts","content":"..."}`, "/etc/hosts"},
		{"NotebookEdit", `{"notebook_path":"/home/a/proj/n.ipynb"}`, "n.ipynb"},
		{"Grep", `{"pattern":"password","path":"/home/a/proj/docs"}`, "docs"},
		{"Bash", `{"command":"rm -rf /"}`, ""},
		{"WebFetch", `{"url":"https://example.com"}`, ""},
	}
	for _, c := range cases {
		got := ToolTarget(Input{ToolName: c.tool, Cwd: "/home/a/proj", ToolInput: json.RawMessage(c.input)})
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.tool, got, c.want)
		}
	}
	long := "/x/" + strings.Repeat("é", 300)
	got := ToolTarget(Input{ToolInput: json.RawMessage(`{"file_path":"` + long + `"}`)})
	if !strings.HasPrefix(got, "…") || len(got) > 204 || !json.Valid([]byte(`"`+got+`"`)) {
		t.Errorf("long path: %q", got)
	}
}
