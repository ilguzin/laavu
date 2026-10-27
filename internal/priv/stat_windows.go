package priv

import "os"

// statUID: file owners are not checked on Windows, where laavu only installs over ssh.
func statUID(os.FileInfo) (int, bool) { return 0, false }
