package priv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ilguzin/laavu/internal/config"
)

// NftRules — правила nftables между агентами: в диапазон портов агента не
// подключаются другие агенты. Сам агент, laavud (прокси dev-серверов) и root —
// подключаются. Правило смотрит на uid владельца исходящего сокета.
func NftRules(agents []config.Agent) string {
	var b strings.Builder
	b.WriteString("# Генерирует laavud priv; правки руками перезапишутся.\n")
	b.WriteString("table inet laavu\ndelete table inet laavu\ntable inet laavu {\n")
	b.WriteString("  chain output {\n    type filter hook output priority 0; policy accept;\n")
	for _, a := range agents {
		var others []string
		for _, o := range agents {
			if o.UID != a.UID {
				others = append(others, fmt.Sprint(o.UID))
			}
		}
		if len(others) == 0 {
			continue
		}
		fmt.Fprintf(&b, "    oif \"lo\" tcp dport %d-%d meta skuid { %s } reject with tcp reset\n",
			a.Ports[0], a.Ports[1], strings.Join(others, ", "))
	}
	b.WriteString("  }\n}\n")
	return b.String()
}

// SyncNftables переписывает /etc/laavu/nftables.nft и загружает правила.
func SyncNftables() error {
	agents, err := Agents()
	if err != nil {
		return err
	}
	path := filepath.Join(ConfDir, "nftables.nft")
	if err := os.WriteFile(path, []byte(NftRules(agents)), 0o644); err != nil {
		return err
	}
	if err := run("nft", "-f", path); err != nil {
		return err
	}
	run("systemctl", "enable", "--now", "laavu-nftables.service")
	return nil
}
