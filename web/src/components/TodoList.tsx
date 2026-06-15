import { useState, useRef, useEffect } from "react";
import type { Todo, TodoStatus } from "../types";
import { updateTodos } from "../store";
import { StatusBar } from "./StatusBar";

// ── Status symbol ─────────────────────────────────────────────────────────────

function StatusSymbol({ status }: { status: TodoStatus }) {
  const s = { fontSize: "var(--chat-font-size)", lineHeight: 1 };
  if (status === "completed")
    return <span className="text-green shrink-0" style={s}>✓</span>;
  if (status === "in_progress")
    return <span className="text-accent shrink-0 animate-pulse" style={s}>◑</span>;
  return <span className="text-text-subtle shrink-0" style={s}>○</span>;
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
      data-test-id="todo-row"
      className="todo-row group"
    >
      {/* Actions — LEFT side, visible on hover */}
      <div className="flex items-center gap-0.5 shrink-0">
        {editing ? (
          <button
            data-test-id="todo-save"
            onClick={commitEdit}
            title="Save"
            className="w-5 h-5 flex items-center justify-center text-green hover:opacity-70 transition-opacity text-[13px] leading-none"
          >
            ✓
          </button>
        ) : (
          <div className="flex items-center gap-0 opacity-0 group-hover:opacity-100 transition-opacity">
            <button
              data-test-id="todo-edit"
              onClick={startEdit}
              title="Edit"
              className="w-5 h-5 flex items-center justify-center text-text-subtle hover:text-accent transition-colors text-[12px] leading-none"
            >
              ✏
            </button>
            <button
              data-test-id="todo-move-up"
              onClick={() => onMove(-1)}
              disabled={index === 0}
              title="Move up"
              className="w-4 h-5 flex items-center justify-center text-text-subtle hover:text-text disabled:opacity-20 transition-colors text-[12px] leading-none"
            >
              ↑
            </button>
            <button
              data-test-id="todo-move-down"
              onClick={() => onMove(1)}
              disabled={index === total - 1}
              title="Move down"
              className="w-4 h-5 flex items-center justify-center text-text-subtle hover:text-text disabled:opacity-20 transition-colors text-[12px] leading-none"
            >
              ↓
            </button>
            <button
              data-test-id="todo-delete"
              onClick={onDelete}
              title="Delete"
              className="w-5 h-5 flex items-center justify-center text-text-subtle hover:text-red transition-colors text-[14px] leading-none"
            >
              ×
            </button>
          </div>
        )}
      </div>

      {/* Status toggle */}
      <button
        data-test-id="todo-status-btn"
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
            data-test-id="todo-edit-input"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={onKey}
            className="field-inline" style={{ fontSize: "var(--chat-font-size)" }}
          />
        ) : (
          <span
            data-test-id="todo-content"
            className={`cursor-default select-none block truncate ${
              todo.status === "completed" ? "line-through text-text-subtle" : "text-text"
            }`} style={{ fontSize: "var(--chat-font-size)" }}
          >
            {todo.content}
          </span>
        )}
      </div>
    </div>
  );
}

// ── New task input row ────────────────────────────────────────────────────────

function AddTaskRow({
  onAdd,
  onCancel,
}: {
  onAdd: (content: string) => void;
  onCancel: () => void;
}) {
  const [draft, setDraft] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  function commit() {
    const trimmed = draft.trim();
    if (trimmed) onAdd(trimmed);
    else onCancel();
  }

  function onKey(e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Enter") commit();
    if (e.key === "Escape") onCancel();
  }

  return (
    <div className="flex items-center gap-1.5 px-2 py-1.5">
      <span className="text-text-subtle shrink-0" style={{ fontSize: "var(--chat-font-size)", lineHeight: 1 }}>○</span>
      <input
        ref={inputRef}
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={onKey}
        onBlur={commit}
        placeholder="New task…"
        className="field-inline flex-1 min-w-0 placeholder:text-text-muted"
        style={{ fontSize: "var(--chat-font-size)" }}
      />
    </div>
  );
}

// ── TodoList panel ────────────────────────────────────────────────────────────

export function TodoList({ sessionID, todos }: { sessionID: string; todos: Todo[] }) {
  const [collapsed, setCollapsed] = useState(false);
  const [addingNew, setAddingNew] = useState(false);

  const completed = todos.filter((t) => t.status === "completed").length;
  const isEmpty = todos.length === 0;

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

  function addTask(content: string) {
    update([...todos, { content, status: "pending", active_form: "" }]);
    setAddingNew(false);
  }

  function clearCompleted() {
    update(todos.filter((t) => t.status !== "completed"));
  }

  function startAdding() {
    setCollapsed(false);
    setAddingNew(true);
  }

  return (
    <div
      data-test-id="todo-list"
      className="shrink-0 border-t border-surface bg-base-subtle/40"
    >
      {/* Header */}
      <div className="flex items-center">
        <button
          data-test-id="todo-list-toggle"
          onClick={() => setCollapsed((c) => !c)}
          className="shrink-0 flex items-center gap-2 px-4 py-2 text-left hover:bg-base-overlay/50 transition-colors"
        >
          <span className={`text-text-subtle text-[11px] transition-transform inline-block ${collapsed ? "" : "rotate-90"}`}>
            ▶
          </span>
          <span className="font-semibold text-text-subtle uppercase tracking-wider" style={{ fontSize: "var(--chat-font-size)" }}>
            Tasks
          </span>
          {!isEmpty && (
            <span className="text-text-muted ml-1" style={{ fontSize: "var(--chat-font-size)" }}>
              {completed}/{todos.length}
            </span>
          )}
        </button>

        {/* Connection + MCP status occupies the empty horizontal strip
            between the "Tasks" toggle and the row's action buttons. */}
        <div className="flex-1 flex items-center justify-center min-w-0 px-3">
          <StatusBar inline />
        </div>

        {/* Clear completed / Add task buttons */}
        {completed > 0 && (
          <button
            data-test-id="todo-clear-completed"
            onClick={clearCompleted}
            title="Clear completed tasks"
            className="px-2 py-1.5 text-text-subtle hover:text-red transition-colors text-[11px] leading-none"
          >
            ✕ Done
          </button>
        )}
        <button
          data-test-id="todo-add-btn"
          onClick={startAdding}
          title="Add task"
          className="px-3 py-2 text-text-subtle hover:text-text transition-colors text-[18px] leading-none"
        >
          +
        </button>
      </div>

      {/* List */}
      {!collapsed && (
        <div className="px-2 pb-2">
          {isEmpty && !addingNew && (
            <p className="text-text-muted px-2 py-1" style={{ fontSize: "var(--chat-font-size)" }}>No tasks yet.</p>
          )}
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
          {addingNew && (
            <AddTaskRow
              onAdd={addTask}
              onCancel={() => setAddingNew(false)}
            />
          )}
        </div>
      )}
    </div>
  );
}
