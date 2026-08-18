//go:build !windows

package session

// TrackProcessTree is a no-op on Unix. Tree teardown there needs no
// runtime registration: KillProcess itself detects process-group
// leaders via getpgid and kills the whole group, and leaderhood is a
// spawn-time attribute the spawning code sets (SysProcAttr.Setpgid),
// not something that can be retrofitted onto a running pid.
func TrackProcessTree(pid int) error { return nil }

// UntrackProcessTree is a no-op on Unix; see TrackProcessTree.
func UntrackProcessTree(pid int) {}
