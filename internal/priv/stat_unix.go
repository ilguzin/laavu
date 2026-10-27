//go:build unix

package priv

import (
	"os"
	"syscall"
)

func statUID(st os.FileInfo) (int, bool) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(s.Uid), true
}
