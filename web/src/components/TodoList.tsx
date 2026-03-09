import { useState, useRef, useEffect } from "react";
import type { Todo, TodoStatus } from "../types";
import { updateTodos } from "../store";

// ── Status symbol ─────────────────────────────────────────────────────────────

function StatusSymbol({ status }: { status: TodoStatus }) {
  if (status === "completed")
    return <span className="text-green text-[15px] leading-none shrink-0">✓</span>;
  if (status === "in_progress")
    return <span className="text-accent text-[15px] leading-none shrink-0 animate-pulse">◑</span>;
  return <span className="text-text-subtle text-[15px] leading-none shrink-0">○</span>;
}

function nextStatus(s: TodoStatus): TodoStatus {
  if (s === "pending") return "in_progress";
  if (s === "in_progress") return "completed";
  return "pending";
}

// ── Single todo row ───────────────────────────────────────────────────────────

function TodoRow({
  todo,
  index,
  total,
  onChange,
  onDelete,
  onMove,
}: {
  todo: Todo;
  index: number;
  total: number;
  onChange: (t: Todo) => void;
  onDelete: () => void;
  onMove: (dir: -1 | 1) => void;
}) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(todo.content);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (editing) inputRef.current?.focus();
  }, [editing]);

  function startEdit() {
    setDraft(todo.content);
    setEditing(true);
  }

  function commitEdit() {
    const trimmed = draft.trim();
    if (trimmed && trimmed !== todo.content) onChange({ ...todo, content: trimmed });
    setEditing(false);
  }

  function cancelEdit() {
    setDraft(todo.content);
    setEditing(false);
  }

  function onKey(e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Enter") commitEdit();
    if (e.key === "Escape") cancelEdit();
  }

  return (
    <div
      data-testid="todo-row"
      className="group flex items-center gap-2 px-3 py-2 rounded-lg hover:bg-base-overlay transition-colors"
    >
      {/* Status toggle */}
      <button
        data-testid="todo-status-btn"
        onClick={() => onChange({ ...todo, status: nextStatus(todo.status) })}
        title={`Status: ${todo.status}`}
        className="shrink-0 hover:opacity-70 transition-opacity"
      >
        <StatusSymbol status={todo.status} />
      </button>

      {/* Content / edit input */}
      <div className="flex-1 min-w-0">
        {editing ? (
          <input
            ref={inputRef}
            data-testid="todo-edit-input"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={onKey}
            className="w-full text-sm bg-canvas border border-accent/40 rounded px-1.5 py-0.5 outline-none text-text"
          />
        ) : (
          <span
            data-testid="todo-content"
            className={`text-sm cursor-default select-none block truncate ${
              todo.status === "completed" ? "line-through text-text-subtle" : "text-text"
            }`}
          >
            {todo.content}
          </span>
        )}
      </div>

      {/* Actions */}
      <div className="flex items-center gap-0.5 shrink-0">
        {editing ? (
          /* Save button — always visible while editing */
          <button
            data-testid="todo-save"
            onClick={commitEdit}
            title="Save"
            className="px-1 text-green hover:opacity-70 transition-opacity text-[14px] leading-none"
          >
            ✓
          </button>
        ) : (
          /* Edit + reorder + delete — visible on row hover */
          <div className="flex items-center gap-0.5 opacity-0 group-hover:opacity-100 transition-opacity">
            <button
              data-testid="todo-edit"
              onClick={startEdit}
              title="Edit"
              className="px-1 text-text-subtle hover:text-accent transition-colors text-[13px] leading-none"
            >
              ✏
            </button>
            <button
              data-testid="todo-move-up"
              onClick={() => onMove(-1)}
              disabled={index === 0}
              title="Move up"
              className="px-0.5 text-text-subtle hover:text-text disabled:opacity-20 transition-colors text-[13px] leading-none"
            >
              ↑
            </button>
            <button
              data-testid="todo-move-down"
              onClick={() => onMove(1)}
              disabled={index === total - 1}
              title="Move down"
              className="px-0.5 text-text-subtle hover:text-text disabled:opacity-20 transition-colors text-[13px] leading-none"
            >
              ↓
            </button>
            <button
              data-testid="todo-delete"
              onClick={onDelete}
              title="Delete"
              className="px-1 text-text-subtle hover:text-red transition-colors text-[13px] leading-none ml-0.5"
            >
              ×
            </button>
          </div>
        )}
      </div>
    </div>
  );
}

// ── TodoList panel ────────────────────────────────────────────────────────────

export function TodoList({ sessionID, todos }: { sessionID: string; todos: Todo[] }) {
  const [collapsed, setCollapsed] = useState(false);

  if (todos.length === 0) return null;

  const completed = todos.filter((t) => t.status === "completed").length;

  function update(next: Todo[]) {
    updateTodos(sessionID, next);
  }

  function changeTodo(i: number, t: Todo) {
    const next = [...todos];
    next[i] = t;
    update(next);
  }

  function deleteTodo(i: number) {
    update(todos.filter((_, idx) => idx !== i));
  }

  function moveTodo(i: number, dir: -1 | 1) {
    const j = i + dir;
    if (j < 0 || j >= todos.length) return;
    const next = [...todos];
    [next[i], next[j]] = [next[j], next[i]];
    update(next);
  }

  return (
    <div
      data-testid="todo-list"
      className="shrink-0 border-t border-surface bg-base-subtle/40"
    >
      {/* Header */}
      <button
        data-testid="todo-list-toggle"
        onClick={() => setCollapsed((c) => !c)}
        className="w-full flex items-center gap-2 px-4 py-2 text-left hover:bg-base-overlay/50 transition-colors"
      >
        <span className={`text-text-subtle text-[11px] transition-transform inline-block ${collapsed ? "" : "rotate-90"}`}>
          ▶
        </span>
        <span className="text-xs font-semibold text-text-subtle uppercase tracking-wider">
          Tasks
        </span>
        <span className="text-xs text-text-muted ml-1">
          {completed}/{todos.length}
        </span>
      </button>

      {/* List */}
      {!collapsed && (
        <div className="px-2 pb-2">
          {todos.map((t, i) => (
            <TodoRow
              key={i}
              todo={t}
              index={i}
              total={todos.length}
              onChange={(t) => changeTodo(i, t)}
              onDelete={() => deleteTodo(i)}
              onMove={(dir) => moveTodo(i, dir)}
            />
          ))}
        </div>
      )}
    </div>
  );
}
