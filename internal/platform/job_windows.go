package platform

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// AssignToNewJobObject creates a new Windows Job Object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, assigns the current process to it,
// and returns. When the parent process exits (even via TerminateProcess),
// the OS kills every process still in the job, preventing orphan child
// processes (bash.exe, go.exe, claude.exe, etc.).
//
// On Windows 8+ the current process may already be in a job that allows
// breakaway — the nested AssignProcessToJobObject call will succeed.
// On older Windows the call fails with ERROR_ACCESS_DENIED; we log and
// skip gracefully rather than crashing.
func AssignToNewJobObject() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}

	proc, err := windows.GetCurrentProcess()
	if err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("GetCurrentProcess: %w", err)
	}

	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job)
		// Nested job assignment fails on older Windows; skip gracefully.
		return fmt.Errorf("AssignProcessToJobObject (nested job?): %w", err)
	}

	// Intentionally leak the job handle: it must stay open for the
	// lifetime of the process so the kill-on-close behaviour triggers
	// when the process exits. The OS closes it on termination.
	return nil
}
