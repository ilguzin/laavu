package worker

import "syscall"

func umask(m int) int { return syscall.Umask(m) }
