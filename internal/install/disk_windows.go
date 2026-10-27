package install

// diskGB: the report is built on the Linux server, never on Windows.
func diskGB(string) (total, free float64, ok bool) { return 0, 0, false }
