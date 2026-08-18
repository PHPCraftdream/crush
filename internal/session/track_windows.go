//go:build windows

package session

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// trackedJob pairs a kill-on-close Job Object handle with the creation
// time of the process the job was created for. The creation time is the
// pid-reuse guard: Windows pids are small and recycled eagerly, so the
// map key alone cannot prove an entry still refers to the process the
// caller means — two processes that successively held the same pid are
// told apart by when they were created.
type trackedJob struct {
	handle    windows.Handle
	createdAt windows.Filetime
}

// trackedJobs maps a spawned child's pid to the kill-on-close Job
// Object TrackProcessTree created for it. Guarded by trackedJobsMu.
var (
	trackedJobs   = make(map[int]trackedJob)
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
	// PROCESS_TERMINATE keeps direct use of the handle possible;
	// PROCESS_QUERY_LIMITED_INFORMATION lets GetProcessTimes record the
	// creation time used as the pid-reuse guard (see trackedJob).
	proc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(pid))
	if err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("TrackProcessTree: OpenProcess %d: %w", pid, err)
	}
	defer windows.CloseHandle(proc)
	var createdAt, exitTime, kernelTime, userTime windows.Filetime
	if err := windows.GetProcessTimes(proc, &createdAt, &exitTime, &kernelTime, &userTime); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("TrackProcessTree: GetProcessTimes %d: %w", pid, err)
	}
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("TrackProcessTree: AssignProcessToJobObject %d: %w", pid, err)
	}
	trackedJobsMu.Lock()
	// A pid can only be re-tracked after the OS recycled it, which means
	// the previous holder exited and its entry is stale by definition.
	// Close the old handle instead of leaking it behind the overwrite;
	// for a KILL_ON_JOB_CLOSE job that also tears down any straggler
	// still inside the old job, which is the intended teardown.
	if old, ok := trackedJobs[pid]; ok {
		_ = windows.CloseHandle(old.handle)
	}
	trackedJobs[pid] = trackedJob{handle: job, createdAt: createdAt}
	trackedJobsMu.Unlock()
	return nil
}

// UntrackProcessTree closes and forgets the Job Object tracked for pid,
// typically once the process has exited and been waited for. Closing
// the last handle of a KILL_ON_JOB_CLOSE job kills whatever is still
// inside it, which is the intended teardown for straggler descendants.
func UntrackProcessTree(pid int) {
	trackedJobsMu.Lock()
	tj, ok := trackedJobs[pid]
	if ok {
		delete(trackedJobs, pid)
	}
	trackedJobsMu.Unlock()
	if ok {
		_ = windows.CloseHandle(tj.handle)
	}
}

// terminateTrackedJob terminates the Job Object tracked for pid, if
// any, and reports whether it did. It is single-shot: the entry is
// removed whether or not TerminateJobObject succeeds, so a job kill
// that fails falls back to KillProcess's taskkill path exactly once
// instead of retrying a broken handle.
//
// pid reuse: the entry is honored only when the process currently
// holding pid was created at the entry's recorded creation time. On a
// mismatch the tracked tree is long gone and the OS has recycled the
// pid for an unrelated process, so the stale job is closed (releasing
// the handle; for a KILL_ON_JOB_CLOSE job that also tears down any
// straggler still inside it) and false is returned, letting
// KillProcess's pid-based paths act on the real, live process instead
// of reporting success for a job that terminated nothing. That check
// cannot be skipped: TerminateJobObject on an empty job or on a job
// whose only member already exited succeeds vacuously (observed
// directly: nil error in both shapes), so without the identity check a
// stale entry would fake a kill and disable the fallback in the same
// stroke.
func terminateTrackedJob(pid int) bool {
	trackedJobsMu.Lock()
	tj, ok := trackedJobs[pid]
	if ok {
		delete(trackedJobs, pid)
	}
	trackedJobsMu.Unlock()
	if !ok {
		return false
	}
	defer windows.CloseHandle(tj.handle)
	if !pidIsSameProcess(pid, tj.createdAt) {
		return false
	}
	return windows.TerminateJobObject(tj.handle, 1) == nil
}

// pidIsSameProcess reports whether the process currently holding pid
// is the one that was created at createdAt. An OpenProcess or
// GetProcessTimes failure fails open (returns true): a pid the OS will
// not even open is gone or inaccessible, so there is no reused process
// to protect, and treating the entry as valid preserves the pre-check
// behavior (the job's straggler teardown still runs). Only a live
// process whose creation time differs is proof of reuse.
func pidIsSameProcess(pid int, createdAt windows.Filetime) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return true
	}
	return creation == createdAt
}
