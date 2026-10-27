package link

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/ilguzin/laavu/internal/names"
)

// Rights the owner grants the console. Defaults are in DefaultRights.
const (
	RightUpdates     = "updates"
	RightMonitoring  = "monitoring"
	RightFirewall    = "firewall"
	RightMaintenance = "maintenance"
	RightDomain      = "domain"
	RightSnapshots   = "snapshots"
	RightAgents      = "agents"
	RightDiagnostics = "diagnostics"
	RightNames       = "names" // agent names in reports; no operations
)

// DefaultRights is what a fresh binding gets.
func DefaultRights() map[string]bool {
	return map[string]bool{
		RightUpdates: true, RightMonitoring: true, RightFirewall: true, RightMaintenance: true,
		RightDomain: true, RightSnapshots: false, RightAgents: false, RightDiagnostics: false, RightNames: false,
	}
}

// RightNamesList returns all rights in a stable order.
func RightNamesList() []string {
	var out []string
	for r := range DefaultRights() {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// OpUnlink is always allowed: it only takes power away from the console.
const OpUnlink = "link.unlink"

// Op describes one operation: the right it needs and how its parameters
// become a laavu command line. Args gets params already decoded and must
// validate every value it puts on the command line.
type Op struct {
	Right string
	Args  func(params json.RawMessage) ([]string, error)
}

type portParams struct {
	Port  int    `json:"port"`
	Proto string `json:"proto,omitempty"` // tcp or udp, default both
}

type agentParams struct {
	Name      string `json:"name"`
	Memory    string `json:"memory,omitempty"`
	Tasks     int    `json:"tasks,omitempty"`
	CPUWeight int    `json:"cpu_weight,omitempty"`
	KeepHome  bool   `json:"keep_home,omitempty"`
}

var memoryRe = regexp.MustCompile(`^[1-9][0-9]{0,5}[KMGT]?$`)

func noParams(args ...string) func(json.RawMessage) ([]string, error) {
	return func(p json.RawMessage) ([]string, error) {
		if len(p) > 0 && string(p) != "{}" && string(p) != "null" {
			return nil, errors.New("this operation takes no parameters")
		}
		return args, nil
	}
}

func decode(p json.RawMessage, v any) error {
	if len(p) == 0 {
		return errors.New("parameters are required")
	}
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parameters: %w", err)
	}
	return nil
}

func firewallArgs(verb string) func(json.RawMessage) ([]string, error) {
	return func(p json.RawMessage) ([]string, error) {
		var v portParams
		if err := decode(p, &v); err != nil {
			return nil, err
		}
		if v.Port < 1 || v.Port > 65535 {
			return nil, errors.New("port must be 1-65535")
		}
		port := fmt.Sprint(v.Port)
		switch v.Proto {
		case "":
		case "tcp", "udp":
			port += "/" + v.Proto
		default:
			return nil, errors.New("proto must be tcp or udp")
		}
		return []string{"firewall", verb, port}, nil
	}
}

func agentArgs(p json.RawMessage) (agentParams, error) {
	var v agentParams
	if err := decode(p, &v); err != nil {
		return v, err
	}
	if err := names.Agent(v.Name); err != nil {
		return v, err
	}
	if v.Memory != "" && !memoryRe.MatchString(v.Memory) {
		return v, errors.New("memory must look like 512M or 2G")
	}
	if v.Tasks < 0 || v.Tasks > 1<<20 || v.CPUWeight < 0 || v.CPUWeight > 10000 {
		return v, errors.New("tasks or cpu_weight out of range")
	}
	return v, nil
}

// Ops is the whole list. Anything else is refused.
var Ops = map[string]Op{
	"check":            {RightMonitoring, noParams("check", "--json")},
	"update.laavud":    {RightUpdates, noParams("update", "laavud")},
	"update.os":        {RightUpdates, noParams("update", "os")},
	"update.claude":    {RightUpdates, noParams("update", "claude")},
	"update.templates": {RightUpdates, noParams("update", "templates")},
	"restart.laavud":   {RightMaintenance, noParams("restart", "laavud")},
	"cleanup":          {RightMaintenance, noParams("cleanup")},
	"firewall.allow":   {RightFirewall, firewallArgs("allow")},
	"firewall.deny":    {RightFirewall, firewallArgs("deny")},
	"diag":             {RightDiagnostics, noParams("diag")},
	"agent.create": {RightAgents, func(p json.RawMessage) ([]string, error) {
		v, err := agentArgs(p)
		if err != nil {
			return nil, err
		}
		a := []string{"agent", "create", v.Name}
		if v.Memory != "" {
			a = append(a, "--memory", v.Memory)
		}
		return a, nil
	}},
	"agent.delete": {RightAgents, func(p json.RawMessage) ([]string, error) {
		v, err := agentArgs(p)
		if err != nil {
			return nil, err
		}
		a := []string{"agent", "delete", v.Name}
		if v.KeepHome {
			a = append(a, "--keep-home")
		}
		return a, nil
	}},
	"agent.limits": {RightAgents, func(p json.RawMessage) ([]string, error) {
		v, err := agentArgs(p)
		if err != nil {
			return nil, err
		}
		a := []string{"agent", "limits", v.Name}
		if v.Memory != "" {
			a = append(a, "--memory", v.Memory)
		}
		if v.Tasks > 0 {
			a = append(a, "--tasks", fmt.Sprint(v.Tasks))
		}
		if v.CPUWeight > 0 {
			a = append(a, "--cpu-weight", fmt.Sprint(v.CPUWeight))
		}
		return a, nil
	}},
}
