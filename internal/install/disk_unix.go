//go:build unix

package install

import "syscall"

// diskGB returns the size and free space of the file system at path, in GiB.
func diskGB(path string) (total, free float64, ok bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0, 0, false
	}
	return float64(st.Blocks) * float64(st.Bsize) / (1 << 30), float64(st.Bavail) * float64(st.Bsize) / (1 << 30), true
}
