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
var todosDescription []byte

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

// mergeTodos combines the model's desired todo list with the current DB state
// applying two protective rules:
//  1. Status protection: a task's status can only advance (pending → in_progress →
//     completed). The model cannot revert a status the user manually set.
//  2. User task preservation: tasks present in the DB but absent from the model's
//     list are kept. This prevents the model from silently deleting tasks the
//     user added manually.
//
// The model can still add new tasks (items in model list not in DB are appended).
func mergeTodos(dbTodos []session.Todo, modelItems []TodoItem) ([]session.Todo, bool) {
	if len(dbTodos) == 0 {
		// Empty DB → accept model's list as-is (fresh start, no user edits to protect).
		todos := make([]session.Todo, len(modelItems))
		for i, item := range modelItems {
			todos[i] = session.Todo{
				Content:    item.Content,
				Status:     session.TodoStatus(item.Status),
				ActiveForm: item.ActiveForm,
			}
		}
		return todos, true
	}

	dbByContent := make(map[string]session.Todo, len(dbTodos))
	for _, t := range dbTodos {
		dbByContent[t.Content] = t
	}
	modelByContent := make(map[string]bool, len(modelItems))
	for _, item := range modelItems {
		modelByContent[item.Content] = true
	}

	var result []session.Todo

	// Process model's items first (preserve model ordering).
	for _, item := range modelItems {
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

	// Append DB tasks the model didn't mention (user-added tasks).
	for _, dbTodo := range dbTodos {
		if !modelByContent[dbTodo.Content] {
			slog.Info("todos tool: keeping user task not mentioned by model", "content", dbTodo.Content)
			result = append(result, dbTodo)
		}
	}

	return result, false
}

func NewTodosTool(sessions session.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TodosToolName,
		string(todosDescription),
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

			todos, isNew := mergeTodos(currentSession.Todos, params.Todos)

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
