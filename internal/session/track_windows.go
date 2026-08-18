//go:build windows

package session

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// trackedJobs maps a spawned child's pid to the kill-on-close Job
// Object TrackProcessTree created for it. Guarded by trackedJobsMu.
var (
	trackedJobs   = make(map[int]windows.Handle)
	trackedJobsMu sync.Mutex
)

// TrackProcessTree puts pid's whole future process tree on a Windows
// Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so KillProcess
// can take the entire tree down with one TerminateJobObject call no
// matter what the PPID chain looks like.
//
// Why taskkill /T alone is not enough: under MSYS2/Git-Bash, spawning
// an external binary routes through a transient intermediary process,
// and the OS records that intermediary — already gone by inspection
// time — as the child's parent. The PPID walk taskkill /T performs
// therefore never reaches the grandchild; this was confirmed by direct
// wmic observation (10/10 deterministic survivor cases, see the comment
// on TestStreamKillUsesTreeKillStillTerminatesChild in
// internal/agent/cliprovider). Job membership, unlike the PPID chain,
// is inherited automatically by every descendant that does not
// explicitly break away, so the job kills the tree even when the
// parent chain is broken.
//
// The KILL_ON_JOB_CLOSE limit doubles as a crash net: the handle is
// held open until the tree is terminated or UntrackProcessTree runs,
// so if crush itself dies without cleanup, the OS closes the handle
// and kills the tree anyway.
//
// Assigning a process that already belongs to another job fails on
// Windows versions without nested-job support (pre-Win8); the error is
// returned so callers can degrade to the taskkill path, preserving the
// pre-Job-Object behavior.
func TrackProcessTree(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("TrackProcessTree: invalid pid %d", pid)
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("TrackProcessTree: CreateJobObject: %w", err)
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
		return fmt.Errorf("TrackProcessTree: SetInformationJobObject: %w", err)
	}
	// PROCESS_SET_QUOTA is what AssignProcessToJobObject checks for;
	// PROCESS_TERMINATE keeps direct use of the handle possible.
	proc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("TrackProcessTree: OpenProcess %d: %w", pid, err)
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("TrackProcessTree: AssignProcessToJobObject %d: %w", pid, err)
	}
	trackedJobsMu.Lock()
	trackedJobs[pid] = job
	trackedJobsMu.Unlock()
	return nil
}

// UntrackProcessTree closes and forgets the Job Object tracked for pid,
// typically once the process has exited and been waited for. Closing
// the last handle of a KILL_ON_JOB_CLOSE job kills whatever is still
// inside it, which is the intended teardown for straggler descendants.
func UntrackProcessTree(pid int) {
	trackedJobsMu.Lock()
	job, ok := trackedJobs[pid]
	if ok {
		delete(trackedJobs, pid)
	}
	trackedJobsMu.Unlock()
	if ok {
		_ = windows.CloseHandle(job)
	}
}

// terminateTrackedJob terminates the Job Object tracked for pid, if
// any, and reports whether it did. It is single-shot: the entry is
// removed whether or not TerminateJobObject succeeds, so a job kill
// that fails falls back to KillProcess's taskkill path exactly once
// instead of retrying a broken handle.
func terminateTrackedJob(pid int) bool {
	trackedJobsMu.Lock()
	job, ok := trackedJobs[pid]
	if ok {
		delete(trackedJobs, pid)
	}
	trackedJobsMu.Unlock()
	if !ok {
		return false
	}
	defer windows.CloseHandle(job)
	return windows.TerminateJobObject(job, 1) == nil
}
