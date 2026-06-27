package tools

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
)

//go:embed todos.md
var todosDescription string

const TodosToolName = "todos"

type TodosParams struct {
	Todos []TodoItem `json:"todos" description:"The updated todo list"`
}

type TodoItem struct {
	Content    string `json:"content" description:"What needs to be done (imperative form)"`
	Status     string `json:"status" description:"Task status: pending, in_progress, or completed"`
	ActiveForm string `json:"active_form" description:"Present continuous form (e.g., 'Running tests')"`
}

type TodosResponseMetadata struct {
	IsNew         bool           `json:"is_new"`
	Todos         []session.Todo `json:"todos"`
	JustCompleted []string       `json:"just_completed,omitempty"`
	JustStarted   string         `json:"just_started,omitempty"`
	Completed     int            `json:"completed"`
	Total         int            `json:"total"`
}

// todoStatusLevel returns a numeric rank for a todo status so we can
// enforce forward-only progression: pending(0) → in_progress(1) → completed(2).
func todoStatusLevel(s session.TodoStatus) int {
	switch s {
	case session.TodoStatusInProgress:
		return 1
	case session.TodoStatusCompleted:
		return 2
	default:
		return 0
	}
}

// mergeTodos merges the model's desired todo list with the current DB state.
//
// Two protective rules apply:
//  1. Status protection: a task's status can only advance
//     (pending → in_progress → completed). The model cannot revert a status
//     the operator manually set.
//  2. Tombstone filtering: if the operator explicitly deleted a todo (its
//     Content string appears in deletedTombstones), the model cannot
//     resurrect it — the item is silently dropped from the result even if
//     the model sends it again.
//
// The model's list is otherwise authoritative — if the model omits a task,
// it is removed.
func mergeTodos(dbTodos []session.Todo, modelItems []TodoItem, deletedTombstones []string) ([]session.Todo, bool) {
	// Build the tombstone set for O(1) lookup.
	tombstoneSet := make(map[string]struct{}, len(deletedTombstones))
	for _, c := range deletedTombstones {
		tombstoneSet[c] = struct{}{}
	}

	if len(dbTodos) == 0 {
		// Empty DB → accept model's list as-is (fresh start), but still
		// honour tombstones so a model that re-sends deleted items on its
		// very first call after deletion is also filtered.
		var todos []session.Todo
		for _, item := range modelItems {
			if _, tombstoned := tombstoneSet[item.Content]; tombstoned {
				slog.Info("todos tool: filtered tombstoned todo (fresh start)",
					"content", item.Content,
				)
				continue
			}
			todos = append(todos, session.Todo{
				Content:    item.Content,
				Status:     session.TodoStatus(item.Status),
				ActiveForm: item.ActiveForm,
			})
		}
		return todos, true
	}

	dbByContent := make(map[string]session.Todo, len(dbTodos))
	for _, t := range dbTodos {
		dbByContent[t.Content] = t
	}

	var result []session.Todo

	// Process model's items: apply tombstone filter then status protection.
	for _, item := range modelItems {
		if _, tombstoned := tombstoneSet[item.Content]; tombstoned {
			slog.Info("todos tool: filtered tombstoned todo",
				"content", item.Content,
			)
			continue
		}
		wantStatus := session.TodoStatus(item.Status)
		if dbTodo, exists := dbByContent[item.Content]; exists {
			// Task exists in DB: don't allow status regression.
			if todoStatusLevel(dbTodo.Status) > todoStatusLevel(wantStatus) {
				slog.Info("todos tool: protecting status from regression",
					"content", item.Content,
					"db_status", dbTodo.Status,
					"model_status", wantStatus,
				)
				wantStatus = dbTodo.Status
			}
		}
		result = append(result, session.Todo{
			Content:    item.Content,
			Status:     wantStatus,
			ActiveForm: item.ActiveForm,
		})
	}

	return result, false
}

func NewTodosTool(sessions session.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TodosToolName,
		todosDescription,
		func(ctx context.Context, params TodosParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for managing todos")
			}

			currentSession, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}

			for _, item := range params.Todos {
				switch item.Status {
				case "pending", "in_progress", "completed":
				default:
					return fantasy.ToolResponse{}, fmt.Errorf("invalid status %q for todo %q", item.Status, item.Content)
				}
			}

			todos, isNew := mergeTodos(currentSession.Todos, params.Todos, currentSession.DeletedTodos)

			slog.Info("todos tool: model updating todos",
				"session", sessionID,
				"prev", currentSession.Todos,
				"merged", todos,
			)

			// Compute response metadata.
			oldStatusByContent := make(map[string]session.TodoStatus)
			for _, todo := range currentSession.Todos {
				oldStatusByContent[todo.Content] = todo.Status
			}
			var justCompleted []string
			var justStarted string
			completedCount, pendingCount, inProgressCount := 0, 0, 0

			for _, todo := range todos {
				switch todo.Status {
				case session.TodoStatusCompleted:
					completedCount++
					if old, existed := oldStatusByContent[todo.Content]; existed && old != session.TodoStatusCompleted {
						justCompleted = append(justCompleted, todo.Content)
					}
				case session.TodoStatusInProgress:
					inProgressCount++
					if old, existed := oldStatusByContent[todo.Content]; !existed || old != session.TodoStatusInProgress {
						if todo.ActiveForm != "" {
							justStarted = todo.ActiveForm
						} else {
							justStarted = todo.Content
						}
					}
				default:
					pendingCount++
				}
			}

			currentSession.Todos = todos
			if _, err = sessions.Save(ctx, currentSession); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to save todos: %w", err)
			}

			response := fmt.Sprintf("Todo list updated successfully.\nStatus: %d pending, %d in progress, %d completed\n",
				pendingCount, inProgressCount, completedCount)
			response += "Todos have been modified successfully. Ensure that you continue to use the todo list to track your progress. Please proceed with the current tasks if applicable."

			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), TodosResponseMetadata{
				IsNew:         isNew,
				Todos:         todos,
				JustCompleted: justCompleted,
				JustStarted:   justStarted,
				Completed:     completedCount,
				Total:         len(todos),
			}), nil
		})
}
