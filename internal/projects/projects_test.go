package projects

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRegisterAndList(t *testing.T) {
	// Create a temporary directory for the test
	tmpDir := t.TempDir()

	// Override the projects file path for testing
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// Test registering a project
	err := Register("/home/user/project1", "/home/user/project1/.crush")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// List projects
	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 1 {
		t.Fatalf("Expected 1 project, got %d", len(projects))
	}

	if projects[0].Path != "/home/user/project1" {
		t.Errorf("Expected path /home/user/project1, got %s", projects[0].Path)
	}

	if projects[0].DataDir != "/home/user/project1/.crush" {
		t.Errorf("Expected data_dir /home/user/project1/.crush, got %s", projects[0].DataDir)
	}

	// Register another project
	err = Register("/home/user/project2", "/home/user/project2/.crush")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, err = List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 2 {
		t.Fatalf("Expected 2 projects, got %d", len(projects))
	}

	// Most recent should be first
	if projects[0].Path != "/home/user/project2" {
		t.Errorf("Expected most recent project first, got %s", projects[0].Path)
	}
}

func TestRegisterUpdatesExisting(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// Register a project
	err := Register("/home/user/project1", "/home/user/project1/.crush")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, _ := List()
	firstAccess := projects[0].LastAccessed

	// Wait a bit and re-register
	time.Sleep(10 * time.Millisecond)

	err = Register("/home/user/project1", "/home/user/project1/.crush-new")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, _ = List()

	if len(projects) != 1 {
		t.Fatalf("Expected 1 project after update, got %d", len(projects))
	}

	if projects[0].DataDir != "/home/user/project1/.crush-new" {
		t.Errorf("Expected updated data_dir, got %s", projects[0].DataDir)
	}

	if !projects[0].LastAccessed.After(firstAccess) {
		t.Error("Expected LastAccessed to be updated")
	}
}

func TestLoadEmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// List before any projects exist
	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 0 {
		t.Errorf("Expected 0 projects, got %d", len(projects))
	}
}

func TestProjectsFilePath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	expected := filepath.Join(tmpDir, "crush", "projects.json")
	actual := projectsFilePath()

	if actual != expected {
		t.Errorf("Expected %s, got %s", expected, actual)
	}
}

func TestRegisterWithParentDataDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// Register a project where .crush is in a parent directory.
	// e.g., working in /home/user/monorepo/packages/app but .crush is at /home/user/monorepo/.crush
	err := Register("/home/user/monorepo/packages/app", "/home/user/monorepo/.crush")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 1 {
		t.Fatalf("Expected 1 project, got %d", len(projects))
	}

	if projects[0].Path != "/home/user/monorepo/packages/app" {
		t.Errorf("Expected path /home/user/monorepo/packages/app, got %s", projects[0].Path)
	}

	if projects[0].DataDir != "/home/user/monorepo/.crush" {
		t.Errorf("Expected data_dir /home/user/monorepo/.crush, got %s", projects[0].DataDir)
	}
}

func TestRegisterWithExternalDataDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// Register a project where .crush is in a completely different location.
	// e.g., project at /home/user/project but data stored at /var/data/crush/myproject
	err := Register("/home/user/project", "/var/data/crush/myproject")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 1 {
		t.Fatalf("Expected 1 project, got %d", len(projects))
	}

	if projects[0].Path != "/home/user/project" {
		t.Errorf("Expected path /home/user/project, got %s", projects[0].Path)
	}

	if projects[0].DataDir != "/var/data/crush/myproject" {
		t.Errorf("Expected data_dir /var/data/crush/myproject, got %s", projects[0].DataDir)
	}
}

// TestRegisterConcurrent reproduces M-2: two concurrent Register calls
// against the same projects.json (simulating two parallel `crush run`
// processes racing to register their own project at startup) must not lose
// either write. Before the fix, Register's Load -> mutate -> Save cycle
// released the package mutex between Load and Save, and had no
// inter-process lock at all, so the second Save could clobber the first
// writer's entry. Run with -race to also catch any remaining data race on
// the in-memory list.
func TestRegisterConcurrent(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			workingDir := fmt.Sprintf("/home/user/project%d", i)
			dataDir := fmt.Sprintf("/home/user/project%d/.crush", i)
			errs[i] = Register(workingDir, dataDir)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Register(%d) failed: %v", i, err)
		}
	}

	projectsList, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projectsList) != n {
		t.Fatalf("Expected %d projects after concurrent Register, got %d (lost writes)", n, len(projectsList))
	}

	seen := make(map[string]bool, n)
	for _, p := range projectsList {
		seen[p.Path] = true
	}
	for i := range n {
		workingDir := fmt.Sprintf("/home/user/project%d", i)
		if !seen[workingDir] {
			t.Errorf("Expected project %s to be present after concurrent Register, but it was lost", workingDir)
		}
	}
}
