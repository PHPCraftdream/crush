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
	}, nil)
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

	result, isNew := mergeTodos(db, model, nil)
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

	result, _ := mergeTodos(db, model, nil)
	if result[0].Status != session.TodoStatusInProgress {
		t.Errorf("expected in_progress, got %s", result[0].Status)
	}
}

// TestMergeTodos_ModelListIsAuthoritative: model list determines which tasks exist.
// If the model omits a task (even one that was in DB), it is removed.
// This allows user deletions to be respected: user deletes → DB shrinks →
// next model call should not re-add deleted tasks.
func TestMergeTodos_ModelListIsAuthoritative(t *testing.T) {
	db := []session.Todo{
		pending("Task A"),
		pending("Task B"),
	}
	// Model only sends Task A, omitting Task B
	model := []TodoItem{item("Task A", "completed")}

	result, _ := mergeTodos(db, model, nil)
	// Model's list is authoritative: only Task A in result
	if len(result) != 1 {
		t.Fatalf("expected 1 todo (model is authoritative), got %d: %+v", len(result), result)
	}
	if result[0].Content != "Task A" {
		t.Errorf("unexpected task: %+v", result[0])
	}
}

// TestMergeTodos_ModelAddsNewTask: model can add tasks not in DB.
func TestMergeTodos_ModelAddsNewTask(t *testing.T) {
	db := []session.Todo{pending("Existing")}
	model := []TodoItem{
		item("Existing", "pending"),
		item("New from model", "pending"),
	}

	result, isNew := mergeTodos(db, model, nil)
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

	result, _ := mergeTodos(db, model, nil)
	if result[0].Content != "C" || result[1].Content != "A" || result[2].Content != "B" {
		t.Errorf("ordering not preserved: %v", result)
	}
}

// TestMergeTodos_EmptyModelList: model passes empty list → result is empty.
// Model's list is authoritative.
func TestMergeTodos_EmptyModelList(t *testing.T) {
	db := []session.Todo{pending("Keep me"), completed("Done task")}
	result, isNew := mergeTodos(db, []TodoItem{}, nil)
	if isNew {
		t.Fatal("expected isNew=false")
	}
	if len(result) != 0 {
		t.Fatalf("expected 0 todos (model is authoritative), got %d: %+v", len(result), result)
	}
}

// TestMergeTodos_DropsTombstones: operator deleted B → model cannot re-add it.
// dbTodos=[A,C,D], modelItems=[A,B,C,D,E], tombstones=[B]
// → result contains A,C,D,E but NOT B.
func TestMergeTodos_DropsTombstones(t *testing.T) {
	dbTodos := []session.Todo{
		pending("A"),
		pending("C"),
		pending("D"),
	}
	modelItems := []TodoItem{
		item("A", "pending"),
		item("B", "pending"), // operator deleted this
		item("C", "pending"),
		item("D", "pending"),
		item("E", "pending"), // new task from model
	}
	tombstones := []string{"B"}

	result, isNew := mergeTodos(dbTodos, modelItems, tombstones)
	if isNew {
		t.Fatal("expected isNew=false when DB not empty")
	}

	contentSet := make(map[string]struct{}, len(result))
	for _, r := range result {
		contentSet[r.Content] = struct{}{}
	}

	if _, found := contentSet["B"]; found {
		t.Error("tombstoned task B should not appear in result")
	}
	for _, want := range []string{"A", "C", "D", "E"} {
		if _, found := contentSet[want]; !found {
			t.Errorf("expected task %q in result, but it was missing", want)
		}
	}
	if len(result) != 4 {
		t.Errorf("expected 4 todos (A,C,D,E), got %d: %+v", len(result), result)
	}
}

// TestMergeTodos_TombstoneLifted: operator adds back a previously tombstoned todo
// by re-including it in their update. mergeTodos should allow it through.
func TestMergeTodos_TombstoneLifted(t *testing.T) {
	// Tombstone for B is in the set, but B is now present in dbTodos (operator
	// re-added it). handleUpdateTodos would have lifted the tombstone before
	// calling Save, so the tombstone set passed to mergeTodos should be empty.
	// This test verifies the no-tombstone path still works correctly.
	dbTodos := []session.Todo{
		pending("A"),
		pending("B"), // re-added by operator (tombstone already lifted)
	}
	modelItems := []TodoItem{
		item("A", "pending"),
		item("B", "pending"),
	}

	result, _ := mergeTodos(dbTodos, modelItems, nil)
	if len(result) != 2 {
		t.Fatalf("expected 2 todos, got %d: %+v", len(result), result)
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
