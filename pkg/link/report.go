package link

// ReportSchema is the version of the Report schema, part of the console contract (docs/link.md).
const ReportSchema = 1

// Report is the laavu check report, filled by laavu check on the server.
type Report struct {
	Schema    int               `json:"schema"`
	Time      int64             `json:"time"`
	Versions  map[string]string `json:"versions"`
	Services  map[string]string `json:"services"`
	Disk      DiskReport        `json:"disk"`
	MemoryMB  MemReport         `json:"memory_mb"`
	Certs     []CertReport      `json:"certs,omitempty"`
	Agents    []AgentReport     `json:"agents"`
	Isolation *IsolationReport  `json:"isolation,omitempty"`
	Updates   int               `json:"os_updates"`
	Reboot    bool              `json:"reboot_required"`
	Tailscale string            `json:"tailscale,omitempty"` // extra or only; empty when off
	URL       string            `json:"url,omitempty"`       // the app address, public_url from laavud.toml
	Problems  []string          `json:"problems"`
}

type DiskReport struct {
	TotalGB float64 `json:"total_gb"`
	FreeGB  float64 `json:"free_gb"`
	UsedPct int     `json:"used_pct"`
}

type MemReport struct {
	Total     int `json:"total"`
	Available int `json:"available"`
}

type CertReport struct {
	Name     string `json:"name"`
	NotAfter int64  `json:"not_after"`
	DaysLeft int    `json:"days_left"`
}

type AgentReport struct {
	Name   string `json:"name"`
	Worker string `json:"worker"`
	Ports  string `json:"ports"`
	Claude string `json:"claude,omitempty"`
}

// IsolationReport is the result of the last check-isolation.
type IsolationReport struct {
	Time   int64 `json:"time"`
	Pairs  int   `json:"pairs"`
	Failed int   `json:"failed"`
}
