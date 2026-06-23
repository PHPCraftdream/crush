import { useState, useRef, useEffect, useMemo, useCallback, memo } from "react";
import { useStore } from "@nanostores/react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import rehypeHighlight from "rehype-highlight";
import type { Message as Msg, ContentPart } from "../types";
import { BrainCircuit, Check, Copy, GitFork, Pencil, RotateCcw, Star, Trash2, BookMarked, ChevronDown, ChevronUp } from "lucide-react";
import { SubAgentBlock } from "./SubAgentBlock";
import {
  $busySessions,
  $messageBlockBreaks,
  toggleMessageSelection,
  updateMessageContent,
  updateMessageThinking,
  deleteMessagePart,
  rerunFromMessage,
  togglePinMessage,
} from "../store";
import { ConfirmDialog } from "./ConfirmDialog";
import { ForkSessionModal } from "./ForkSessionModal";

// Stable arrays — never recreated
const MD_REMARK = [remarkGfm, remarkBreaks];
const MD_REHYPE = [rehypeHighlight];

// ── Tiny utilities ────────────────────────────────────────────────────────────

function formatDuration(s: number) {
  if (s < 60) return `${s.toFixed(1)}s`;
  return `${Math.floor(s / 60)}m ${Math.floor(s % 60)}s`;
}

// formatEventTime returns "HH:MM:SS" in the operator's local timezone
// (Intl picks the system TZ automatically). The value is shown next to
// every event header — message, tool group, tool row, thinking row — so
// the operator can correlate the chat against logs / external runs.
// Epoch seconds; we keep 1s precision intentionally (matches the int64
// column in SQLite).
function formatEventTime(epochSec: number | undefined): string {
  if (!epochSec || epochSec <= 0) return "";
  const d = new Date(epochSec * 1000);
  // Force 24h + seconds; locale may otherwise drop seconds in toLocaleTimeString.
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  const ss = String(d.getSeconds()).padStart(2, "0");
  return `${hh}:${mm}:${ss}`;
}

// formatEventDateTime — for tooltips: full local "YYYY-MM-DD HH:MM:SS".
function formatEventDateTime(epochSec: number | undefined): string {
  if (!epochSec || epochSec <= 0) return "";
  const d = new Date(epochSec * 1000);
  const yyyy = d.getFullYear();
  const mo = String(d.getMonth() + 1).padStart(2, "0");
  const dd = String(d.getDate()).padStart(2, "0");
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  const ss = String(d.getSeconds()).padStart(2, "0");
  const tz = -d.getTimezoneOffset();
  const tzSign = tz >= 0 ? "+" : "-";
  const tzAbs = Math.abs(tz);
  const tzH = String(Math.floor(tzAbs / 60)).padStart(2, "0");
  const tzM = String(tzAbs % 60).padStart(2, "0");
  return `${yyyy}-${mo}-${dd} ${hh}:${mm}:${ss} UTC${tzSign}${tzH}:${tzM}`;
}

const TimeBadge = memo(function TimeBadge({ epochSec }: { epochSec: number | undefined }) {
  const text = formatEventTime(epochSec);
  if (!text) return null;
  return (
    <span
      className="text-xs text-text-subtle font-mono tabular-nums"
      title={formatEventDateTime(epochSec)}
    >
      {text}
    </span>
  );
});

function extractText(parts: ContentPart[]) {
  return parts.filter(p => p.type === "text").map(p => (p as any).Text).join("\n");
}

function extractThinking(parts: ContentPart[]) {
  return parts.filter(p => p.type === "thinking").map(p => (p as any).Thinking).join("\n");
}

function formatJSON(s: string) {
  try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; }
}

// ── Leaf components ───────────────────────────────────────────────────────────

const DurationBadge = memo(function DurationBadge({ message }: { message: Msg }) {
  const isFinished = useMemo(() => message.Parts.some(p => p.type === "finish"), [message.Parts]);
  const busy = useStore($busySessions);
  const isLive = !isFinished && busy.has(message.SessionID);
  const [elapsed, setElapsed] = useState(0);

  useEffect(() => {
    if (!isLive) return;
    const start = message.CreatedAt;
    const tick = () => setElapsed(Date.now() / 1000 - start);
    tick();
    const id = setInterval(tick, 100);
    return () => clearInterval(id);
  }, [isLive, message.CreatedAt]);

  const duration = isFinished || !isLive ? message.UpdatedAt - message.CreatedAt : elapsed;
  if (duration < 0.5) return null;
  return <span className="text-xs text-text-subtle font-mono tabular-nums">{formatDuration(duration)}</span>;
});

const CopyButton = memo(function CopyButton({ text, className = "", label = "Copy" }: { text: string; className?: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = useCallback(() => {
    navigator.clipboard.writeText(text).then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); });
  }, [text]);
  return (
    <button onClick={copy} title={label} className={`btn-copy ${className}`}>
      {copied ? <><Check size={14} className="text-green" /><span className="text-green">Copied</span></> : <><Copy size={14} /><span>{label}</span></>}
    </button>
  );
});

// ── EditForm — owns all editing state and handlers ────────────────────────────

const EditForm = memo(function EditForm({
  initialValue,
  rows,
  className,
  onSave,
  onCancel,
}: {
  initialValue: string;
  rows: number;
  className: string;
  onSave: (text: string) => void;
  onCancel: () => void;
}) {
  const [value, setValue] = useState(initialValue);
  const taRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    if (taRef.current) {
      taRef.current.focus();
      taRef.current.selectionStart = taRef.current.value.length;
      taRef.current.style.height = "auto";
      taRef.current.style.height = taRef.current.scrollHeight + "px";
    }
  }, []);

  const onInput = useCallback((e: React.ChangeEvent<HTMLTextAreaElement>) => {
    setValue(e.target.value);
    e.target.style.height = "auto";
    e.target.style.height = e.target.scrollHeight + "px";
  }, []);

  const commit = useCallback(() => onSave(value), [onSave, value]);

  const onKey = useCallback((e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Escape") onCancel();
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) onSave(value);
  }, [onSave, onCancel, value]);

  return (
    <div className="flex flex-col gap-2">
      <textarea ref={taRef} value={value} onChange={onInput} onKeyDown={onKey} rows={rows} className={className} style={{ overflow: "hidden" }} />
      <div className="flex gap-2">
        <button onClick={onCancel} className="btn-ghost-sm">Cancel</button>
        <button onClick={commit} className="px-3 py-1 text-xs btn-primary">Save</button>
      </div>
    </div>
  );
});

// ── Hover action strips — only mounted when hovered ───────────────────────────

const UserHoverActions = memo(function UserHoverActions({
  messageID, copyText, isLastUserMsg, isPinned, onEdit, onDelete, onFork,
}: {
  messageID: string; copyText: string; isLastUserMsg: boolean; isPinned: boolean;
  onEdit: () => void; onDelete: () => void; onFork: () => void;
}) {
  const handleRerun = useCallback(() => rerunFromMessage(messageID), [messageID]);
  const handlePin   = useCallback(() => togglePinMessage(messageID, !isPinned), [messageID, isPinned]);
  return (
    <div className="flex items-center gap-1.5">
      {copyText && <CopyButton text={copyText} />}
      {isLastUserMsg && <button onClick={handleRerun} title="Rerun" className="btn-icon"><RotateCcw size={13} /></button>}
      <button onClick={handlePin}   title={isPinned ? "Unpin" : "Pin message"} className={`p-1.5 transition-colors rounded ${isPinned ? "text-yellow" : "text-text-subtle hover:text-yellow"}`}><Star size={13} fill={isPinned ? "currentColor" : "none"} /></button>
      <button onClick={onFork}      title="Fork session"                       className="btn-icon"><GitFork size={13} /></button>
      <button onClick={onEdit}      title="Edit"                               className="btn-icon"><Pencil  size={13} /></button>
      <button onClick={onDelete}    title="Delete"                             className="btn-icon-danger"><Trash2 size={13} /></button>
    </div>
  );
});

const AssistantHoverActions = memo(function AssistantHoverActions({
  message, copyText, copyAll, onEdit, onDelete, onFork,
}: {
  message: Msg; copyText: string; copyAll: string;
  onEdit: () => void; onDelete: () => void; onFork: () => void;
}) {
  const handlePin = useCallback(() => togglePinMessage(message.ID, !message.Pinned), [message.ID, message.Pinned]);
  return (
    <div className="flex items-center gap-1 w-full">
      <div className="flex items-center gap-1.5">
        {copyText && <CopyButton text={copyText} />}
        {copyAll  && <CopyButton text={copyAll}  label="Copy all" />}
        <button onClick={handlePin} title={message.Pinned ? "Unpin" : "Pin message"} className={`p-1.5 transition-colors rounded ${message.Pinned ? "text-yellow" : "text-text-subtle hover:text-yellow"}`}><Star size={13} fill={message.Pinned ? "currentColor" : "none"} /></button>
        <button onClick={onFork}    title="Fork session" className="btn-icon"><GitFork size={13} /></button>
        <button onClick={onEdit}    title="Edit"         className="btn-icon"><Pencil  size={13} /></button>
        <button onClick={onDelete}  title="Delete"       className="btn-icon-danger"><Trash2 size={13} /></button>
      </div>
      <div className="flex items-center gap-2 ml-auto">
        <TimeBadge epochSec={message.CreatedAt} />
        <DurationBadge message={message} />
        {message.Model && (
          <span className="text-xs text-text-subtle font-mono flex items-center gap-1">
            {message.Model}
            {message.ReasoningEffort && (message.Provider === "anthropic" || message.Provider === "local-cli") && (
              <span className="px-1 py-0.5 rounded bg-base-subtle text-text-muted font-mono text-[10px]" title={`Reasoning effort: ${message.ReasoningEffort}`}>
                {message.ReasoningEffort === "low" ? "L" : message.ReasoningEffort === "medium" ? "M" : message.ReasoningEffort === "high" ? "H" : "X"}
              </span>
            )}
          </span>
        )}
      </div>
    </div>
  );
});

// ── Tool blocks ───────────────────────────────────────────────────────────────

// FileWriteTools — tool names whose input.content is the full file body and
// whose result metadata carries a unified diff. The UI hides the bulk content,
// shows just the file path on the call, and renders a coloured diff on the
// result instead of the noisy "<result>\nFile successfully written: …\n</result>"
// blob the model sees.
const FileWriteTools = new Set(["write", "edit", "multiedit"]);

// DiffLine — one rendered line of a unified diff. We do not require a real
// parser: a single pass over the string is enough to colour +/- lines and
// drop the noisy "+++"/"---" file headers.
type DiffLineKind = "add" | "del" | "ctx" | "hdr" | "meta";
interface DiffLine { kind: DiffLineKind; text: string }

function parseUnifiedDiff(diff: string): DiffLine[] {
  const out: DiffLine[] = [];
  const lines = diff.split(/\r?\n/);
  for (const line of lines) {
    if (line.startsWith("+++") || line.startsWith("---")) { out.push({ kind: "meta", text: line }); continue; }
    if (line.startsWith("@@")) { out.push({ kind: "hdr", text: line }); continue; }
    if (line.startsWith("+"))  { out.push({ kind: "add", text: line }); continue; }
    if (line.startsWith("-"))  { out.push({ kind: "del", text: line }); continue; }
    out.push({ kind: "ctx", text: line });
  }
  // Trim trailing blank line(s) — split adds an empty entry when diff ends with \n.
  while (out.length && out[out.length - 1].text === "" && out[out.length - 1].kind === "ctx") out.pop();
  return out;
}

interface WriteMetadata { diff?: string; additions?: number; removals?: number }

function safeParseWriteMetadata(raw: string | undefined): WriteMetadata | null {
  if (!raw) return null;
  try { return JSON.parse(raw) as WriteMetadata; } catch { return null; }
}

interface WriteInput { file_path?: string; content?: string; old_string?: string; new_string?: string }

function safeParseWriteInput(raw: string): WriteInput {
  try { return JSON.parse(raw) as WriteInput; } catch { return {}; }
}

const DiffView = memo(function DiffView({ diff, additions, removals }: { diff: string; additions?: number; removals?: number }) {
  const lines = useMemo(() => parseUnifiedDiff(diff), [diff]);
  return (
    <div data-test-id="diff-view" className="diff-view">
      {(additions !== undefined || removals !== undefined) && (
        <div className="diff-stats text-xs mb-1">
          {additions !== undefined && <span className="text-green">+{additions}</span>}{" "}
          {removals !== undefined && <span className="text-red">−{removals}</span>}
        </div>
      )}
      <pre className="diff-body">
        {lines.map((l, i) => (
          <div key={i} className={`diff-line diff-${l.kind}`}>{l.text || " "}</div>
        ))}
      </pre>
    </div>
  );
});

const ToolCallBlock = memo(function ToolCallBlock({ name, input, finished }: { name: string; input: string; finished: boolean }) {
  const isFileWrite = FileWriteTools.has(name);
  const writeInput  = isFileWrite ? safeParseWriteInput(input) : null;
  const formatted   = useMemo(() => formatJSON(input), [input]);
  const [rawOpen, setRawOpen] = useState(false);

  return (
    <div data-test-id="tool-call" className="tool-block my-2">
      <div className="flex items-center justify-between gap-2 mb-2">
        <div className="flex items-center gap-2">
          <span className="text-xs text-text-subtle">⚡</span>
          <span className="text-mauve font-semibold text-sm">{name}</span>
          {writeInput?.file_path && <span className="text-text-muted text-xs font-mono truncate max-w-[36em]">{writeInput.file_path}</span>}
          {!finished && <span data-test-id="tool-call-running" className="text-text-subtle text-xs animate-pulse">running…</span>}
        </div>
        <CopyButton text={input} />
      </div>
      {isFileWrite ? (
        <div className="tool-output-details">
          <button
            type="button"
            onClick={() => setRawOpen((v) => !v)}
            aria-expanded={rawOpen}
            className="cursor-pointer text-text-subtle text-xs select-none bg-transparent border-0 p-0"
          >
            {rawOpen ? "hide raw input" : "show raw input"}
          </button>
          {rawOpen && <pre className="tool-output mt-1">{formatted}</pre>}
        </div>
      ) : (
        <pre className="tool-output">{formatted}</pre>
      )}
    </div>
  );
});

const ToolResultBlock = memo(function ToolResultBlock({ name, content, isError, metadata }: { name: string; content: string; isError: boolean; metadata?: string }) {
  const isFileWrite = FileWriteTools.has(name);
  const meta        = isFileWrite ? safeParseWriteMetadata(metadata) : null;
  const hasDiff     = !!meta?.diff;
  const [diffOpen, setDiffOpen] = useState(true);

  return (
    <div data-test-id="tool-result" className="tool-block my-2 opacity-80">
      <div className="flex items-center justify-between gap-2 mb-2">
        <div className="flex items-center gap-2">
          <span className="text-xs text-text-subtle">↩</span>
          <span className="text-text-muted font-semibold text-sm">{name}</span>
          {isError && <span data-test-id="tool-result-error" className="badge-error">error</span>}
        </div>
        <CopyButton text={hasDiff ? meta!.diff! : content} />
      </div>
      {hasDiff ? (
        <div className="tool-output-details">
          <button
            type="button"
            onClick={() => setDiffOpen((v) => !v)}
            aria-expanded={diffOpen}
            className="cursor-pointer text-text-subtle text-xs select-none bg-transparent border-0 p-0"
          >
            diff <span className="text-green">+{meta!.additions ?? 0}</span>{" "}
            <span className="text-red">−{meta!.removals ?? 0}</span>
          </button>
          {diffOpen && <DiffView diff={meta!.diff!} additions={meta!.additions} removals={meta!.removals} />}
        </div>
      ) : (
        <pre className="tool-output">{content}</pre>
      )}
    </div>
  );
});

// ── Tool activity group ───────────────────────────────────────────────────────
//
// A "tool" visual block (a burst of tool_call/tool_result parts between two
// stretches of text or thinking) gets rendered as a vertical accordion list
// instead of a flat stack of cards. Rules:
//
//   • One row per pair: a tool_call is paired with its tool_result by
//     ToolCallID. Unpaired results (rare — orphaned from an aborted earlier
//     call) get their own row with no head.
//
//   • Row open-state: `open = userOverride ?? isCurrent`.
//     isCurrent = last row in the list. While the turn is live, that's the
//     in-flight action. After the turn ends, the last action stays expanded
//     by default (most relevant to look at), prior rows stay collapsed.
//     A click freezes the override and the auto-rule no longer touches it.
//
//   • Group open-state: open by default (user wanted to "see the process").
//     Manual collapse via the chevron in the header — never auto-collapsed.

// formatActionArgs — one-line preview shown in the collapsed row header.
// Picks the most useful identifying argument for known tool names so the
// reader scans by file path / command / pattern, not by raw JSON.
function formatActionArgs(name: string, input: string): string {
  if (!input) return "";
  let parsed: Record<string, unknown> = {};
  try { parsed = JSON.parse(input) as Record<string, unknown>; } catch { return ""; }
  const s = (k: string) => typeof parsed[k] === "string" ? (parsed[k] as string) : "";
  switch (name) {
    case "bash":      return s("command");
    case "view":      return s("file_path") || s("path") || s("filePath");
    case "write":
    case "edit":
    case "multiedit": return s("file_path");
    case "glob":      return s("pattern");
    case "grep":      return [s("pattern"), s("path")].filter(Boolean).join(" · ");
    case "ls":        return s("path");
    case "fetch":     return s("url");
    case "download":  return s("url");
    case "agent":     return s("prompt") || s("description");
    default: {
      // First string value in the object as a sensible fallback.
      for (const v of Object.values(parsed)) if (typeof v === "string" && v) return v;
      return "";
    }
  }
}

type ActionItem =
  | {
      kind: "tool";
      callPart?: ContentPart & { type: "tool_call"; ID: string; Name: string; Input: string; Finished: boolean };
      resultPart?: ContentPart & { type: "tool_result"; ToolCallID: string; Name: string; Content: string; IsError: boolean; Metadata?: string };
      idx: number;
      key: string;
      // CreatedAt of the source message that contributed this row. For a
      // paired call+result we keep the EARLIER timestamp (the call) — the
      // result's createdAt arrives later but the user thinks of the action
      // by when it started.
      createdAt?: number;
    }
  | {
      kind: "thinking";
      text: string;
      idx: number;
      key: string;
      createdAt?: number;
    };

interface ActionRowProps {
  item: ActionItem;
  isCurrent: boolean;
  // When set, the auto-current rule is suppressed for this render. Comes
  // from ToolActivityGroup after a manual collapse: re-expanding the group
  // should leave every row closed until the user explicitly clicks one,
  // or a live tool arrival kicks the group back into auto mode.
  suppressAutoCurrent: boolean;
}

const ActionRow = memo(function ActionRow({ item, isCurrent, suppressAutoCurrent }: ActionRowProps) {
  // override:
  //   undefined → follow auto-rule (open iff isCurrent, AND auto isn't suppressed)
  //   true / false → user pinned, ignore auto-rule from now on
  const [override, setOverride] = useState<boolean | undefined>(undefined);
  const effectiveCurrent = suppressAutoCurrent ? false : isCurrent;
  const open = override ?? effectiveCurrent;
  const toggle = useCallback(() => setOverride(!open), [open]);

  if (item.kind === "thinking") {
    // Thinking rows live alongside tool rows in the accordion. Same
    // open/close + auto-current rules; collapsed header shows a one-line
    // preview of the model's reasoning so the operator can scan the
    // chain without expanding every row.
    const preview = item.text.replace(/\s+/g, " ").trim();
    return (
      <div data-test-id="action-row" className="action-row">
        <button
          type="button"
          onClick={toggle}
          aria-expanded={open}
          data-test-id="action-row-toggle"
          className="action-row-head"
          title={preview || "thinking"}
        >
          <span className="text-accent/70 shrink-0"><BrainCircuit size={13} /></span>
          <span className="text-accent/80 font-semibold text-sm shrink-0">thinking</span>
          <span className="text-text font-mono text-sm truncate flex-1 min-w-0">
            {preview || "—"}
          </span>
          <TimeBadge epochSec={item.createdAt} />
          <span className="text-text-subtle shrink-0">
            {open ? <ChevronUp size={14} /> : <ChevronDown size={14} />}
          </span>
        </button>
        {open && (
          <div className="action-row-body">
            <pre className="tool-output whitespace-pre-wrap">{item.text}</pre>
          </div>
        )}
      </div>
    );
  }

  const call    = item.callPart;
  const result  = item.resultPart;
  const name    = call?.Name ?? result?.Name ?? "tool";
  const subject = call ? formatActionArgs(call.Name, call.Input) : "";
  const running = !!call && !call.Finished && !result;
  const errored = !!result?.IsError;
  return (
    <div data-test-id="action-row" className="action-row">
      <button
        type="button"
        onClick={toggle}
        aria-expanded={open}
        data-test-id="action-row-toggle"
        className="action-row-head"
        title={subject || name}
      >
        <span className="text-xs text-text-subtle shrink-0">⚡</span>
        <span className="text-mauve font-semibold text-sm shrink-0">{name}</span>
        {/* Subject (file path, command, pattern, …) is the primary readable
            label of the row — same size and weight as the tool name so a
            collapsed accordion immediately tells the operator WHICH file
            each action touched, not just that "an edit happened". */}
        <span className="text-text font-mono text-sm truncate flex-1 min-w-0">
          {subject || "—"}
        </span>
        {running && <span className="text-text-subtle text-xs animate-pulse shrink-0">running…</span>}
        {errored && <span className="badge-error shrink-0">error</span>}
        <TimeBadge epochSec={item.createdAt} />
        <span className="text-text-subtle shrink-0">
          {open ? <ChevronUp size={14} /> : <ChevronDown size={14} />}
        </span>
      </button>
      {open && (
        <div className="action-row-body">
          {call && <ToolCallBlock name={call.Name} input={call.Input} finished={call.Finished} />}
          {result && <ToolResultBlock name={result.Name} content={result.Content} isError={result.IsError} metadata={result.Metadata} />}
        </div>
      )}
    </div>
  );
});

interface ToolActivityGroupProps {
  items: { part: ContentPart; idx: number; createdAt?: number }[];
  live: boolean;
  // True when this group is the most recent activity in the transcript
  // (i.e. nothing rendered after it). When false, the auto-rule collapses
  // the group — the user moved on, the work is in the past.
  isCurrent: boolean;
  // Epoch-seconds timestamp of the first message that contributed to this
  // burst. Shown as a "HH:MM:SS" badge in the group header so the operator
  // can correlate the activity with logs.
  startedAt?: number;
}

export const ToolActivityGroup = memo(function ToolActivityGroup({ items, live, isCurrent, startedAt }: ToolActivityGroupProps) {
  // Group open/close state machine.
  //
  // The default collapsed state follows `isCurrent`: the most recent group
  // stays expanded, older groups fold automatically as soon as a newer
  // renderitem (typically a user message or the next group) appears in the
  // transcript. The user's own toggle wins via `collapsedOverride` and
  // sticks until explicitly changed.
  //
  // `suppressAuto` is a one-shot latch for the inside-of-the-group auto-
  // current rule: after a manual collapse, re-expanding the group leaves
  // EVERY row closed (including the row that would normally auto-open
  // because it's the last one). The latch is released the moment a NEW
  // tool arrives within the same group — that's a live event the user
  // wants to see — or when the group transitions back to isCurrent.
  //
  // When the body is unmounted (`collapsed === true`) every ActionRow's
  // internal override state is reset (React unmount discards useState), so
  // collapsing the group also drops any per-row pins the user had set —
  // "сворачивание идёт с уничтожением контента" verbatim.
  const [collapsedOverride, setCollapsedOverride] = useState<boolean | undefined>(undefined);
  const [suppressAuto, setSuppressAuto] = useState(false);
  // Sticky-current latch. While the session is `live` (busy), buildRenderItems
  // can flicker isCurrent off for a frame whenever a streaming assistant
  // message is briefly tool-less (thinking-only). Letting that flicker
  // collapse the container would unmount its body and lose all per-row
  // pins/state. So:
  //   - isCurrent === true  → latch to true.
  //   - isCurrent === false AND live === true → keep prior latch value.
  //   - isCurrent === false AND live === false → release latch (true → false).
  // Once the agent finishes (live = false) the natural !isCurrent auto-collapse
  // kicks in again and old groups fold.
  const [stickyCurrent, setStickyCurrent] = useState(isCurrent);
  useEffect(() => {
    if (isCurrent) setStickyCurrent(true);
    else if (!live) setStickyCurrent(false);
  }, [isCurrent, live]);
  const effectiveCurrent = stickyCurrent;
  const autoCollapsed = !effectiveCurrent;
  const collapsed = collapsedOverride ?? autoCollapsed;

  const prevItemsLen = useRef(items.length);
  const prevIsCurrent = useRef(isCurrent);
  useEffect(() => {
    const grew = items.length > prevItemsLen.current;
    const becameCurrent = isCurrent && !prevIsCurrent.current;
    if (grew || becameCurrent) {
      // Either a new tool arrived in this group, or this group just became
      // the latest activity. Drop the user's collapse pin (live event takes
      // precedence) and clear the suppress latch so the new last row auto-
      // opens via its isCurrent prop in ActionRow.
      setCollapsedOverride(undefined);
      setSuppressAuto(false);
    }
    prevItemsLen.current = items.length;
    prevIsCurrent.current = isCurrent;
  }, [items.length, isCurrent]);

  // Any collapse (manual OR automatic via isCurrent → false) latches
  // suppressAuto. The body unmounts, which already discards each ActionRow's
  // override state; setting the latch ensures that on the next expand the
  // auto-current rule doesn't re-open the last row either — every row stays
  // closed until the user clicks one, or a fresh tool arrival clears the
  // latch via the effect above.
  useEffect(() => {
    if (collapsed) setSuppressAuto(true);
  }, [collapsed]);

  const toggle = useCallback(() => {
    setCollapsedOverride((prev) => {
      const cur = prev ?? autoCollapsed;
      return !cur;
    });
  }, [autoCollapsed]);

  // Pair calls and results by ToolCallID. Order is the order calls appear.
  // An "agent" tool_call is passed through unchanged — SubAgentBlock owns
  // its own rendering and lives outside the row schema. Same for the matching
  // tool_result if any.
  const { actions, rawAgentParts } = useMemo(() => {
    const actions: ActionItem[] = [];
    const rawAgentParts: { part: ContentPart; idx: number }[] = [];
    const indexByCallID = new Map<string, number>();
    for (const { part, idx, createdAt } of items) {
      if (part.type === "thinking") {
        const text = (part as { type: "thinking"; Thinking: string }).Thinking ?? "";
        actions.push({ kind: "thinking", text, idx, key: `think-${idx}`, createdAt });
      } else if (part.type === "tool_call") {
        if (part.Name === "agent") { rawAgentParts.push({ part, idx }); continue; }
        const a: ActionItem = { kind: "tool", callPart: part, idx, key: `call-${part.ID}`, createdAt };
        indexByCallID.set(part.ID, actions.length);
        actions.push(a);
      } else if (part.type === "tool_result") {
        if (part.Name === "agent") { rawAgentParts.push({ part, idx }); continue; }
        const pos = indexByCallID.get(part.ToolCallID);
        if (pos !== undefined) {
          const slot = actions[pos];
          if (slot.kind === "tool") slot.resultPart = part;
          // intentionally keep the earlier createdAt: that's when the action began
        } else {
          actions.push({ kind: "tool", resultPart: part, idx, key: `res-${part.ToolCallID}-${idx}`, createdAt });
        }
      }
    }
    return { actions, rawAgentParts };
  }, [items]);

  const tally = useMemo(() => {
    const counts = new Map<string, number>();
    let thinkingCount = 0;
    for (const a of actions) {
      if (a.kind === "thinking") { thinkingCount++; continue; }
      const n = a.callPart?.Name ?? a.resultPart?.Name ?? "tool";
      counts.set(n, (counts.get(n) ?? 0) + 1);
    }
    const parts = [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 4)
      .map(([k, v]) => `${v} ${k}`);
    if (thinkingCount > 0) parts.push(`${thinkingCount} thinking`);
    return parts.join(" · ");
  }, [actions]);

  // SubAgent parts always render in their own (existing) component, in their
  // original order at the END of the group so they don't get swallowed by the
  // collapse toggle. They're rare in practice — usually zero per group.
  const renderAgents = () => rawAgentParts.map(({ part, idx }) => {
    if (part.type === "tool_call") {
      let prompt = "";
      try { prompt = (JSON.parse(part.Input) as { prompt?: string }).prompt ?? part.Input; } catch { prompt = part.Input; }
      return <SubAgentBlock key={`a-${idx}`} messageID="" toolCallID={part.ID} prompt={prompt} />;
    }
    return null;
  });

  // Nothing to show — neither real actions nor sub-agent calls. Without this
  // guard buildRenderItems-driven empty bursts would render a stray "0 actions"
  // header with an empty body.
  if (actions.length === 0 && rawAgentParts.length === 0) return null;

  // Group contained ONLY agent tool_calls (e.g. fan-out steps). The accordion
  // header is meaningless ("0 actions"), so render the sub-agent blocks
  // inline without the group wrapper.
  if (actions.length === 0) {
    return <>{renderAgents()}</>;
  }

  return (
    <div data-test-id="tool-activity-group" className="tool-activity-group">
      <button
        type="button"
        onClick={toggle}
        data-test-id="tool-activity-toggle"
        className="tool-activity-head"
        aria-expanded={!collapsed}
        title={collapsed ? "Expand actions" : "Collapse actions"}
      >
        <span className="text-mauve font-semibold text-sm shrink-0">
          {actions.length} {actions.length === 1 ? "action" : "actions"}
        </span>
        {tally && <span className="text-text-subtle text-xs truncate flex-1 min-w-0">{tally}</span>}
        {!tally && <span className="flex-1" />}
        <TimeBadge epochSec={startedAt} />
        {live && <span className="text-text-subtle text-xs animate-pulse shrink-0">live</span>}
        <span className="text-text-subtle shrink-0">
          {collapsed ? <ChevronDown size={16} /> : <ChevronUp size={16} />}
        </span>
      </button>
      {!collapsed && (
        <div className="tool-activity-body">
          {actions.map((a, i) => (
            <ActionRow
              key={a.key}
              item={a}
              isCurrent={i === actions.length - 1}
              suppressAutoCurrent={suppressAuto}
            />
          ))}
          {renderAgents()}
          {/* Sticky bottom collapse strip, same affordance as ThinkingPart:
              long tool bursts are tedious to fold when the toggle is only
              at the top. Reuses the .thinking-collapse-bottom style. */}
          <button
            type="button"
            onClick={toggle}
            data-test-id="tool-activity-collapse-bottom"
            className="thinking-collapse-bottom"
            title="Collapse actions"
          >
            <ChevronUp size={14} />
            <span>Collapse actions</span>
          </button>
        </div>
      )}
    </div>
  );
});

const TextBlock = memo(function TextBlock({ text, isUser }: { text: string; isUser: boolean }) {
  if (isUser) return <span className="whitespace-pre-wrap">{text}</span>;
  return (
    <div className="md">
      <ReactMarkdown remarkPlugins={MD_REMARK} rehypePlugins={MD_REHYPE}>{text}</ReactMarkdown>
    </div>
  );
});

// ── ThinkingPart — owns its own edit/delete state ─────────────────────────────

const ThinkingPart = memo(function ThinkingPart({ thinking, messageID, partIndex, done }: { thinking: string; messageID: string; partIndex: number; done: boolean }) {
  const [editing, setEditing] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  // Controlled open-state (not <details>) so we can render a sticky
  // collapse strip at the bottom — long reasoning blocks are tedious to
  // close when the toggle is only at the top.
  const [open, setOpen] = useState(false);

  const closeEdit  = useCallback(() => setEditing(false), []);
  const openDel    = useCallback((e: React.MouseEvent) => { e.preventDefault(); e.stopPropagation(); setConfirmDelete(true); }, []);
  const openEditEv = useCallback((e: React.MouseEvent) => { e.preventDefault(); e.stopPropagation(); setEditing(true); }, []);
  const toggleOpen = useCallback(() => setOpen((v) => !v), []);

  const handleSave = useCallback((text: string) => {
    if (text && text !== thinking) updateMessageThinking(messageID, text);
    setEditing(false);
  }, [thinking, messageID]);

  const handleConfirmDelete = useCallback(() => {
    deleteMessagePart(messageID, partIndex);
    setConfirmDelete(false);
  }, [messageID, partIndex]);

  // While the model is still thinking we always render fully (no toggle);
  // streaming reasoning hidden behind a click is useless.
  if (!done) {
    return (
      <div data-test-id="thinking-card" className="thinking-card">
        <div className="thinking-card-header">
          <BrainCircuit size={15} className="text-accent/70 shrink-0 animate-pulse" />
          <span data-test-id="thinking-label">Thinking…</span>
        </div>
        {thinking && (
          <pre data-test-id="thinking-content" className="px-4 pb-3 font-mono whitespace-pre-wrap text-text-subtle leading-relaxed max-h-40 overflow-y-auto border-t border-surface/50" style={{ fontSize: "var(--chat-font-size)" }}>
            {thinking}
          </pre>
        )}
      </div>
    );
  }

  return (
    <div data-test-id="thinking-card" className="thinking-card-done group">
      <button
        type="button"
        onClick={toggleOpen}
        data-test-id="thinking-toggle"
        className="thinking-toggle w-full text-left"
        aria-expanded={open}
      >
        <span className="text-accent/70"><BrainCircuit size={18} /></span>
        <span data-test-id="thinking-label">Thoughts</span>
        <div className="ml-auto flex items-center gap-0.5 hover-reveal" onClick={(e) => e.stopPropagation()}>
          <CopyButton text={thinking} className="px-1.5 py-1 text-xs" />
          <button onClick={openEditEv} title="Edit thinking"   className="btn-icon-sm"><Pencil size={13} /></button>
          <button onClick={openDel}    title="Delete thinking" className="btn-icon-sm-danger"><Trash2 size={13} /></button>
        </div>
        <span className="text-text-subtle ml-1 shrink-0">
          {open ? <ChevronUp size={16} /> : <ChevronDown size={16} />}
        </span>
      </button>
      {open && confirmDelete && (
        <ConfirmDialog
          title="Delete thinking"
          message="The model's reasoning will be removed from this message. This cannot be undone."
          confirmLabel="Delete"
          onConfirm={handleConfirmDelete}
          onCancel={() => setConfirmDelete(false)}
        />
      )}
      {open && (editing ? (
        <div className="p-4 bg-base-overlay border-t border-surface">
          <EditForm
            initialValue={thinking}
            rows={6}
            className="w-full bg-base-subtle border border-accent/40 text-text-muted rounded-lg px-4 py-3 text-[14px] font-mono leading-relaxed resize-none outline-none focus:border-accent"
            onSave={handleSave}
            onCancel={closeEdit}
          />
        </div>
      ) : (
        <>
          <pre data-test-id="thinking-content" className="p-5 bg-base-overlay font-mono whitespace-pre-wrap overflow-x-auto text-text-muted border-t border-surface leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
            {thinking}
          </pre>
          {/* Sticky bottom collapse strip: stays visible as the user scrolls
              past the reasoning so closing it is one click away — no need to
              scroll back up to the header toggle. */}
          <button
            type="button"
            onClick={toggleOpen}
            data-test-id="thinking-collapse-bottom"
            className="thinking-collapse-bottom"
            title="Collapse thinking"
          >
            <ChevronUp size={14} />
            <span>Collapse thinking</span>
          </button>
        </>
      ))}
    </div>
  );
});

// StandaloneThinking is the renderer for a thinking part that was extracted
// out of its assistant message because the surrounding tool parts were
// folded into a cross-message ToolRun. It reuses ThinkingPart so the edit /
// delete / copy / sticky-collapse affordances stay identical to the in-
// message rendering. Wrapped in a small flex container so it sits in the
// chat scroll list at the same horizontal padding as a message row.
export function StandaloneThinking({ messageID, partIndex, thinking, done }: { messageID: string; partIndex: number; thinking: string; done: boolean }) {
  return (
    <div className="msg-row flex flex-col px-5 py-2">
      <div className="w-full min-w-0">
        <ThinkingPart messageID={messageID} partIndex={partIndex} thinking={thinking} done={done} />
      </div>
    </div>
  );
}

// StandaloneText renders a small assistant-prose chunk that was extracted
// from an otherwise-tool-bearing assistant message. These usually carry one
// short sentence ("Let me check the next file") between actions in a long
// tool burst — the action itself moves into the cross-message accordion,
// the words stay on their own row so the reader still sees the model's
// running narrative. Uses TextBlock for the markdown render path so links,
// code spans and bullet lists all work identically to a normal assistant
// message body.
export function StandaloneText({ text }: { text: string }) {
  return (
    <div className="msg-row flex flex-col px-5 py-2 text-text leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
      <div className="w-full min-w-0">
        <TextBlock text={text} isUser={false} />
      </div>
    </div>
  );
}

// ── Block grouping for zebra pattern ──────────────────────────────────────

const EMPTY_BREAKS = new Set<number>();

type BlockKind = "thinking" | "text" | "tool" | "other";

interface VisualBlock {
  kind: BlockKind;
  items: { part: ContentPart; idx: number }[];
  thinkingDone: boolean;
}

function classifyPart(part: ContentPart): BlockKind {
  switch (part.type) {
    case "thinking":    return "thinking";
    case "text":        return "text";
    case "tool_call":
    case "tool_result": return "tool";
    default:            return "other";
  }
}

function groupPartsIntoBlocks(parts: ContentPart[], breaks: Set<number>): VisualBlock[] {
  const blocks: VisualBlock[] = [];
  let cur: VisualBlock | null = null;

  for (let i = 0; i < parts.length; i++) {
    const kind = classifyPart(parts[i]);
    if (!cur || cur.kind !== kind || breaks.has(i)) {
      cur = { kind, items: [], thinkingDone: false };
      blocks.push(cur);
    }
    cur.items.push({ part: parts[i], idx: i });
  }

  for (let b = 0; b < blocks.length; b++) {
    if (blocks[b].kind === "thinking") {
      blocks[b].thinkingDone = blocks.slice(b + 1).some(
        bb => bb.kind === "text" || bb.kind === "other"
      );
    }
  }

  return blocks;
}

// ── Part router ───────────────────────────────────────────────────────────────

const Part = memo(function Part({ part, index, isUser, messageID, thinkingDone, partialWorkDone }: { part: ContentPart; index: number; isUser: boolean; messageID: string; thinkingDone: boolean; partialWorkDone: boolean }) {
  switch (part.type) {
    case "text":     return <TextBlock text={part.Text} isUser={isUser} />;
    case "thinking": return <ThinkingPart thinking={part.Thinking} messageID={messageID} partIndex={index} done={thinkingDone} />;
    case "tool_call": {
      if (part.Name === "agent") {
        let prompt = "";
        try { prompt = JSON.parse(part.Input).prompt ?? part.Input; } catch { prompt = part.Input; }
        return <SubAgentBlock messageID={messageID} toolCallID={part.ID} prompt={prompt} />;
      }
      return <ToolCallBlock name={part.Name} input={part.Input} finished={part.Finished} />;
    }
    case "tool_result": {
      if (part.Name === "agent") return null;
      return <ToolResultBlock name={part.Name} content={part.Content} isError={part.IsError} metadata={part.Metadata} />;
    }
    // Fork patch: render explicit error/empty finish parts so a failed turn is
    // never silently rendered as a blank block. See CHANGELOG.fork.md.
    case "finish": {
      // Stream stalled AFTER substantive work happened (tool calls, text,
      // reasoning) is not a real error — the model did useful things, the
      // provider just went quiet on the tail and the watchdog cut the stream.
      // Render that case as a soft amber "paused" notice so the user sees the
      // work above as legitimate, not framed inside a scary red failure block.
      if (part.Reason === "error" && part.Message === "Stream stalled" && partialWorkDone) {
        return <StreamPausedBlock details={part.Details} />;
      }
      if (part.Reason === "error" || part.Reason === "canceled") {
        return <FinishErrorBlock reason={part.Reason} message={part.Message} details={part.Details} />;
      }
      return null;
    }
    default: return null;
  }
});

// Fork patch: visible block for error / canceled finish parts (replaces the
// previous "render nothing" behaviour that produced blank assistant bubbles).
const FinishErrorBlock = memo(function FinishErrorBlock({ reason, message, details }: { reason: string; message: string; details: string }) {
  const title = message || (reason === "canceled" ? "Canceled" : "Error");
  return (
    <div data-test-id="finish-error" className="tool-block my-2 border-red/40 bg-red/[6%]">
      <div className="flex items-center gap-2 mb-1">
        <span className="text-red font-semibold text-sm">{title}</span>
        <span className="badge-error">{reason}</span>
      </div>
      {details && <pre className="tool-output whitespace-pre-wrap">{details}</pre>}
    </div>
  );
});

// StreamPausedBlock — soft notice for a watchdog stall that happened AFTER
// the model already produced substantive output. The work above is intact;
// only the tail of the stream was cut. Distinct from FinishErrorBlock to
// stop the UI from screaming "ERROR" when nothing in the turn actually
// failed — the user can re-prompt to continue with the inventory.
const StreamPausedBlock = memo(function StreamPausedBlock({ details }: { details: string }) {
  return (
    <div data-test-id="stream-paused" className="tool-block my-2 border-yellow/40 bg-yellow/[6%]">
      <div className="flex items-center gap-2 mb-1">
        <span className="text-yellow font-semibold text-sm">Stream paused</span>
        <span className="text-text-subtle text-xs">watchdog cut the tail · work above is intact</span>
      </div>
      {details && <pre className="tool-output whitespace-pre-wrap text-text-subtle">{details}</pre>}
    </div>
  );
});

// ── Message content areas — own editing state ─────────────────────────────────

const UserContent = memo(function UserContent({
  message, editing, onSaveEdit, onCancelEdit,
}: {
  message: Msg;
  editing: boolean;
  onSaveEdit: (text: string) => void;
  onCancelEdit: () => void;
}) {
  if (editing) {
    return (
      <EditForm
        initialValue={extractText(message.Parts)}
        rows={1}
        className="msg-bubble-user-edit text-[18px] min-w-[300px]"
        onSave={onSaveEdit}
        onCancel={onCancelEdit}
      />
    );
  }
  return (
    <div className="msg-bubble-user" style={{ fontSize: "var(--chat-font-size)" }}>
      {message.Parts.map((part, i) => <Part key={i} part={part} index={i} isUser messageID={message.ID} thinkingDone={false} />)}
    </div>
  );
});

const AssistantContent = memo(function AssistantContent({
  message, editing, onSaveEdit, onCancelEdit,
}: {
  message: Msg;
  editing: boolean;
  onSaveEdit: (text: string) => void;
  onCancelEdit: () => void;
}) {
  const breakMap = useStore($messageBlockBreaks);
  const breaks = useMemo(() => breakMap.get(message.ID) ?? EMPTY_BREAKS, [breakMap, message.ID]);
  const blocks = useMemo(() => groupPartsIntoBlocks(message.Parts, breaks), [message.Parts, breaks]);
  const busy = useStore($busySessions);

  // Fork patch: detect assistant messages that produced no visible content
  // (no text / tool_call / tool_result / thinking). This used to render as a
  // blank block in the WUI. We now show an explicit "empty response" notice
  // for finished turns and a "streaming…" placeholder for live turns.
  const hasVisibleContent = useMemo(
    () => message.Parts.some(p =>
      p.type === "text" || p.type === "tool_call" || p.type === "tool_result" || p.type === "thinking"
    ),
    [message.Parts]
  );
  const isFinished = useMemo(() => message.Parts.some(p => p.type === "finish"), [message.Parts]);
  const finishPart = useMemo(
    () => message.Parts.find(p => p.type === "finish") as (typeof message.Parts[number] & { type: "finish"; Reason: string; Message: string; Details: string }) | undefined,
    [message.Parts]
  );

  if (editing) {
    return (
      <EditForm
        initialValue={extractText(message.Parts)}
        rows={4}
        className="field-textarea text-[16px]"
        onSave={onSaveEdit}
        onCancel={onCancelEdit}
      />
    );
  }
  if (!hasVisibleContent) {
    // A turn that's still in flight may already carry a finish-part in the DB
    // (created speculatively by recovery / cancel paths and rewritten the
    // moment the first real delta arrives). Suppressing the "Empty response"
    // notice while busy avoids the flash where the placeholder shows for a
    // few hundred milliseconds and then disappears under the actual answer.
    const isLive = busy.has(message.SessionID);
    if (isFinished && !isLive) {
      const reason = finishPart?.Reason ?? "unknown";
      const msg = finishPart?.Message || "Empty response";
      const details = finishPart?.Details || "The provider closed the stream without returning any content. Please retry.";
      return (
        <div className="text-text leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
          <FinishErrorBlock reason={reason} message={msg} details={details} />
        </div>
      );
    }
    return (
      <div className="text-text-subtle leading-relaxed italic" style={{ fontSize: "var(--chat-font-size)" }}>
        {isLive ? "streaming…" : "(no content)"}
      </div>
    );
  }
  // partialWorkDone — used by the finish-part router to pick StreamPausedBlock
  // (soft amber) over FinishErrorBlock (red) when the watchdog stall happened
  // after the model already produced something substantive.
  const partialWorkDone = hasVisibleContent;
  const isLive = busy.has(message.SessionID);
  return (
    <div className="text-text leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
      {blocks.map((block, bi) => (
        <div key={bi} className={bi > 0 ? "msg-block-sep" : undefined}>
          {block.kind === "tool" ? (
            // Whole tool burst is rendered through the accordion group: one
            // collapsible row per call+result pair, last row open by default
            // (the "current action"), prior rows collapsed. User can pin any
            // row open/closed and the auto-rule stops touching that row.
            <ToolActivityGroup items={block.items} live={isLive} />
          ) : (
            block.items.map(({ part, idx }) => (
              <Part key={idx} part={part} index={idx} isUser={false} messageID={message.ID} thinkingDone={block.thinkingDone} partialWorkDone={partialWorkDone} />
            ))
          )}
        </div>
      ))}
    </div>
  );
});

// ── SummaryMessage ────────────────────────────────────────────────────────────

const SummaryMessage = memo(function SummaryMessage({ message }: { message: Msg }) {
  const text = useMemo(() => extractText(message.Parts), [message.Parts]);
  const isFinished = useMemo(() => message.Parts.some(p => p.type === "finish"), [message.Parts]);
  const [editing, setEditing] = useState(false);
  const [open, setOpen] = useState(false);

  const handleSave = useCallback((newText: string) => {
    if (newText && newText !== text) updateMessageContent(message.ID, newText);
    setEditing(false);
  }, [message.ID, text]);

  return (
    <div className="px-8 py-3">
      <div className="summary-card">
        <div className="summary-header">
          <BookMarked size={15} className="text-yellow shrink-0" />
          <span className="text-sm font-semibold text-yellow">Context condensed</span>
          <span className="ml-auto text-xs text-text-muted font-mono flex items-center gap-1">
            {message.Model}
            {message.ReasoningEffort && (message.Provider === "anthropic" || message.Provider === "local-cli") && (
              <span className="px-1 py-0.5 rounded bg-base-subtle text-text-muted font-mono text-[10px]" title={`Reasoning effort: ${message.ReasoningEffort}`}>
                {message.ReasoningEffort === "low" ? "L" : message.ReasoningEffort === "medium" ? "M" : message.ReasoningEffort === "high" ? "H" : "X"}
              </span>
            )}
          </span>
          {isFinished && <DurationBadge message={message} />}
          {isFinished && (
            <button onClick={() => setEditing(e => !e)} title="Edit summary" className="btn-icon-sm ml-1">
              <Pencil size={13} />
            </button>
          )}
        </div>
        <div className="group">
          <button
            type="button"
            onClick={() => setOpen((v) => !v)}
            aria-expanded={open}
            className="summary-toggle w-full text-left bg-transparent border-0"
          >
            {open ? "Hide summary ▾" : "Show summary ▸"}
          </button>
          {open && (editing ? (
            <div className="summary-body chat-font">
              <EditForm
                initialValue={text}
                rows={4}
                className="field-textarea"
                onSave={handleSave}
                onCancel={() => setEditing(false)}
              />
            </div>
          ) : text ? (
            <div className="summary-body md">
              <ReactMarkdown remarkPlugins={MD_REMARK} rehypePlugins={MD_REHYPE}>{text}</ReactMarkdown>
            </div>
          ) : null)}
        </div>
      </div>
    </div>
  );
});

// ── Message ───────────────────────────────────────────────────────────────────

export interface MessageProps {
  message: Msg;
  onDeleteRequest: (id: string) => void;
  onRangeSelect: (index: number) => void;
  selectionActive: boolean;
  isLastUserMsg: boolean;
  isSelected: boolean;
  forkDefaultTitle: string;
  sessionID: string;
  index: number;
}

export const Message = memo(function Message({
  message, onDeleteRequest, onRangeSelect, selectionActive, isLastUserMsg, isSelected, forkDefaultTitle, sessionID, index,
}: MessageProps) {
  if (message.Hidden) return null;
  if (message.IsSummaryMessage) return <SummaryMessage message={message} />;

  const isUser = message.Role === "user";

  const copyText     = useMemo(() => extractText(message.Parts), [message.Parts]);
  const copyThinking = useMemo(() => !isUser ? extractThinking(message.Parts) : "", [isUser, message.Parts]);
  const copyAll      = useMemo(() => {
    if (!copyThinking) return "";
    return copyText ? `<thinking>\n${copyThinking}\n</thinking>\n\n${copyText}` : copyThinking;
  }, [copyText, copyThinking]);
  const hasContent   = useMemo(() => !isUser && message.Parts.some(p => ["text","tool_call","tool_result","finish"].includes(p.type)), [isUser, message.Parts]);

  const [editing, setEditing] = useState(false);
  const [forking, setForking] = useState(false);
  const [hovered, setHovered] = useState(false);

  const handleMouseEnter = useCallback(() => setHovered(true),  []);
  const handleMouseLeave = useCallback(() => setHovered(false), []);
  const handleForkOpen   = useCallback(() => setForking(true),  []);
  const handleForkClose  = useCallback(() => setForking(false), []);
  const handleEditOpen   = useCallback(() => setEditing(true),  []);
  const handleEditClose  = useCallback(() => setEditing(false), []);
  const handleDelete     = useCallback(() => onDeleteRequest(message.ID), [onDeleteRequest, message.ID]);

  const handleSaveEdit = useCallback((text: string) => {
    if (text && text !== extractText(message.Parts)) updateMessageContent(message.ID, text);
    setEditing(false);
  }, [message.ID, message.Parts]);

  const handleCheckboxClick = useCallback((e: React.MouseEvent) => {
    if (e.shiftKey) {
      e.preventDefault(); // Prevent text selection between clicks
      onRangeSelect(index);
    }
    else { toggleMessageSelection(message.ID); }
  }, [message.ID, index, onRangeSelect]);

  // Checkbox is always in DOM (reserves layout space), opacity-0 when not relevant
  const checkboxVisible = selectionActive || isSelected || hovered;

  return (
    <div
      id={`msg-${message.ID}`}
      data-msg-role={isUser ? "user" : "assistant"}
      className={`msg-row flex flex-col px-5 py-3 transition-colors ${isSelected ? "bg-accent/5" : ""} ${message.Pinned ? "border-l-4 border-yellow/60 bg-yellow/[5%]" : ""}`}
      onMouseEnter={handleMouseEnter}
      onMouseLeave={handleMouseLeave}
    >
      <div className={`flex gap-3 ${isUser ? "justify-end" : "justify-start"}`}>
        <div
          className={`msg-checkbox-wrap ${checkboxVisible ? "opacity-100" : "opacity-0 pointer-events-none"}`}
          style={{ order: isUser ? 1 : -1 }}
          onClick={handleCheckboxClick}
        >
          <div className={`msg-checkbox ${isSelected ? "bg-accent border-accent" : "border-text-subtle/50 hover:border-accent"}`}>
            {isSelected && <Check size={10} className="text-white shrink-0" />}
          </div>
        </div>
        {isUser ? (
          <div className="max-w-[80%]">
            <UserContent message={message} editing={editing} onSaveEdit={handleSaveEdit} onCancelEdit={handleEditClose} />
          </div>
        ) : (
          <div className="w-full min-w-0">
            <AssistantContent message={message} editing={editing} onSaveEdit={handleSaveEdit} onCancelEdit={handleEditClose} />
          </div>
        )}
      </div>

      {/* Action strip — fixed-height row; interactive controls only mounted on hover */}
      {!editing && (
        <div className={`msg-actions ${isUser ? "justify-end" : "justify-start"}`}>
          {/* Star is always visible for pinned messages, regardless of hover state */}
          {message.Pinned && (
            <Star
              size={13}
              className={`text-yellow shrink-0 ${isUser ? "order-last ml-2" : "order-first mr-2"}`}
              fill="currentColor"
            />
          )}
          {hovered && (
            isUser ? (
              <UserHoverActions
                messageID={message.ID}
                copyText={copyText}
                isLastUserMsg={isLastUserMsg}
                isPinned={message.Pinned}
                onEdit={handleEditOpen}
                onDelete={handleDelete}
                onFork={handleForkOpen}
              />
            ) : hasContent ? (
              <AssistantHoverActions
                message={message}
                copyText={copyText}
                copyAll={copyAll}
                onEdit={handleEditOpen}
                onDelete={handleDelete}
                onFork={handleForkOpen}
              />
            ) : null
          )}
        </div>
      )}

      {forking && (
        <ForkSessionModal sessionID={sessionID} defaultTitle={forkDefaultTitle} onClose={handleForkClose} />
      )}
    </div>
  );
});
