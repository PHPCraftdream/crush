package tools

import (
	"testing"

	"github.com/charmbracelet/crush/internal/session"
)

// helpers
func pending(content string) session.Todo {
	return session.Todo{Content: content, Status: session.TodoStatusPending}
}

func inProgress(content string) session.Todo {
	return session.Todo{Content: content, Status: session.TodoStatusInProgress}
}

func completed(content string) session.Todo {
	return session.Todo{Content: content, Status: session.TodoStatusCompleted}
}

func item(content, status string) TodoItem {
	return TodoItem{Content: content, Status: status}
}

// TestMergeTodos_FreshStart: empty DB → accept model list as-is, isNew=true
func TestMergeTodos_FreshStart(t *testing.T) {
	result, isNew := mergeTodos(nil, []TodoItem{
		item("Task A", "pending"),
		item("Task B", "in_progress"),
	})
	if !isNew {
		t.Fatal("expected isNew=true for empty DB")
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 todos, got %d", len(result))
	}
	if result[0].Content != "Task A" || result[0].Status != session.TodoStatusPending {
		t.Errorf("unexpected result[0]: %+v", result[0])
	}
	if result[1].Content != "Task B" || result[1].Status != session.TodoStatusInProgress {
		t.Errorf("unexpected result[1]: %+v", result[1])
	}
}

// TestMergeTodos_StatusProtection: model cannot revert a status that advanced.
func TestMergeTodos_StatusProtection(t *testing.T) {
	db := []session.Todo{
		completed("Deploy"),   // user marked completed
		inProgress("Testing"), // user started
	}
	// Model tries to send these back as pending
	model := []TodoItem{
		item("Deploy", "pending"),
		item("Testing", "pending"),
	}

	result, isNew := mergeTodos(db, model)
	if isNew {
		t.Fatal("expected isNew=false")
	}
	if result[0].Status != session.TodoStatusCompleted {
		t.Errorf("Deploy: expected completed, got %s", result[0].Status)
	}
	if result[1].Status != session.TodoStatusInProgress {
		t.Errorf("Testing: expected in_progress, got %s", result[1].Status)
	}
}

// TestMergeTodos_StatusAdvance: model CAN advance a status.
func TestMergeTodos_StatusAdvance(t *testing.T) {
	db := []session.Todo{pending("Write tests")}
	model := []TodoItem{item("Write tests", "in_progress")}

	result, _ := mergeTodos(db, model)
	if result[0].Status != session.TodoStatusInProgress {
		t.Errorf("expected in_progress, got %s", result[0].Status)
	}
}

// TestMergeTodos_UserTaskPreservation: DB tasks absent from model are kept.
func TestMergeTodos_UserTaskPreservation(t *testing.T) {
	db := []session.Todo{
		pending("User added task"),
		pending("Model task"),
	}
	// Model only sends its task, omitting the user-added one
	model := []TodoItem{item("Model task", "completed")}

	result, _ := mergeTodos(db, model)
	// Should have both: model task first (from model ordering), then preserved user task
	if len(result) != 2 {
		t.Fatalf("expected 2 todos, got %d: %+v", len(result), result)
	}
	// Find user-added task
	found := false
	for _, r := range result {
		if r.Content == "User added task" {
			found = true
		}
	}
	if !found {
		t.Error("user-added task was deleted by model")
	}
}

// TestMergeTodos_ModelAddsNewTask: model can add tasks not in DB.
func TestMergeTodos_ModelAddsNewTask(t *testing.T) {
	db := []session.Todo{pending("Existing")}
	model := []TodoItem{
		item("Existing", "pending"),
		item("New from model", "pending"),
	}

	result, isNew := mergeTodos(db, model)
	if isNew {
		t.Fatal("expected isNew=false when DB not empty")
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 todos, got %d", len(result))
	}
	if result[1].Content != "New from model" {
		t.Errorf("expected new task to be appended: %+v", result[1])
	}
}

// TestMergeTodos_OrderingPreserved: model ordering is used for known tasks.
func TestMergeTodos_OrderingPreserved(t *testing.T) {
	db := []session.Todo{pending("A"), pending("B"), pending("C")}
	// Model reorders them
	model := []TodoItem{item("C", "pending"), item("A", "pending"), item("B", "pending")}

	result, _ := mergeTodos(db, model)
	if result[0].Content != "C" || result[1].Content != "A" || result[2].Content != "B" {
		t.Errorf("ordering not preserved: %v", result)
	}
}

// TestMergeTodos_EmptyModelList: model passes empty list → DB tasks all preserved.
func TestMergeTodos_EmptyModelList(t *testing.T) {
	db := []session.Todo{pending("Keep me"), completed("Done task")}
	result, isNew := mergeTodos(db, []TodoItem{})
	if isNew {
		t.Fatal("expected isNew=false")
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 todos preserved, got %d", len(result))
	}
}

// TestTodoStatusLevel checks the ordering helpers.
func TestTodoStatusLevel(t *testing.T) {
	if todoStatusLevel(session.TodoStatusPending) != 0 {
		t.Error("pending should be 0")
	}
	if todoStatusLevel(session.TodoStatusInProgress) != 1 {
		t.Error("in_progress should be 1")
	}
	if todoStatusLevel(session.TodoStatusCompleted) != 2 {
		t.Error("completed should be 2")
	}
}
