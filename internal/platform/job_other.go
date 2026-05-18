//go:build !windows

package platform

// AssignToNewJobObject is a no-op on non-Windows platforms.
// On Unix systems, child process group cleanup is handled by
// the shell builtin / signal propagation.
func AssignToNewJobObject() error {
	return nil
}
