import { useStore } from "@nanostores/react";
import { $activeSessionID, $sessions, $permissionRules, $yolo, setYolo, getPermissionRules, togglePermissionRule, deletePermissionRule, fetchPermissionRules } from "../store";
import { X, Trash2, Check, Zap, Plus } from "lucide-react";

export function PermissionsModal({ onClose }: { onClose: () => void }) {
  const activeSessionID = useStore($activeSessionID);
  const sessions = useStore($sessions);
  const yolo = useStore($yolo);
  const rulesMap = useStore($permissionRules);

  const session = sessions.find((s) => s.ID === activeSessionID);
  const rules = activeSessionID ? (rulesMap.get(activeSessionID) || []) : [];

  // Fetch rules when modal opens
  if (activeSessionID && !rulesMap.has(activeSessionID)) {
    fetchPermissionRules(activeSessionID);
  }

  const handleToggleYolo = (enabled: boolean) => {
    setYolo(enabled);
  };

  return (
    <div className="fixed inset-0 z-[10000] flex items-center justify-center bg-black/40 backdrop-blur-sm">
      <div className="bg-canvas border border-surface rounded-2xl shadow-2xl w-[min(700px,90vw)] max-h-[80vh] flex flex-col">
        {/* Header */}
        <div className="flex items-center justify-between p-6 border-b border-surface">
          <div>
            <h2 className="text-xl font-bold text-text">Permissions</h2>
            {session && (
              <p className="text-sm text-text-subtle mt-1">{session.Title}</p>
            )}
          </div>
          <button
            onClick={onClose}
            className="text-text-subtle hover:text-text transition-colors p-1 rounded hover:bg-base-overlay"
          >
            <X size={20} />
          </button>
        </div>

        {/* Content */}
        <div className="flex-1 overflow-y-auto p-6">
          {/* YOLO Toggle */}
          <div className="mb-6 p-4 bg-yellow/5 border border-yellow/30 rounded-xl">
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-3">
                <Zap className={yolo ? "text-yellow" : "text-text-subtle"} size={20} />
                <div>
                  <div className="font-semibold text-text">YOLO Mode</div>
                  <div className="text-sm text-text-subtle">
                    {yolo
                      ? "All permissions are automatically approved"
                      : "Individual permission rules are applied"}
                  </div>
                </div>
              </div>
              <button
                onClick={() => handleToggleYolo(!yolo)}
                className={`relative w-14 h-7 rounded-full transition-colors ${
                  yolo ? "bg-yellow" : "bg-surface"
                }`}
              >
                <div
                  className={`absolute top-1 w-5 h-5 bg-white rounded-full shadow transition-transform ${
                    yolo ? "translate-x-7" : "translate-x-1"
                  }`}
                />
              </button>
            </div>
          </div>

          {/* Rules List */}
          {!yolo && (
            <>
              <div className="flex items-center justify-between mb-4">
                <h3 className="text-lg font-semibold text-text">Permission Rules</h3>
                <button
                  className="flex items-center gap-1.5 text-sm text-accent hover:text-accent/80 transition-colors disabled:opacity-50"
                  disabled={!activeSessionID}
                >
                  <Plus size={16} />
                  Add Rule
                </button>
              </div>

              {rules.length === 0 ? (
                <div className="text-center py-12 text-text-subtle">
                  <p className="mb-2">No permission rules set for this session</p>
                  <p className="text-sm">
                    Use "Allow always" when prompted to create automatic rules
                  </p>
                </div>
              ) : (
                <div className="space-y-2">
                  {rules.map((rule) => (
                    <PermissionRuleCard
                      key={rule.ID}
                      rule={rule}
                      sessionID={activeSessionID || ""}
                      onToggle={() => togglePermissionRule(activeSessionID || "", rule.ID)}
                      onDelete={() => deletePermissionRule(activeSessionID || "", rule.ID)}
                    />
                  ))}
                </div>
              )}
            </>
          )}
        </div>

        {/* Footer */}
        <div className="p-6 border-t border-surface flex justify-end">
          <button
            onClick={onClose}
            className="px-4 py-2 rounded-xl bg-base-overlay border border-surface text-text-subtle hover:text-text transition-colors"
          >
            Close
          </button>
        </div>
      </div>
    </div>
  );
}

function PermissionRuleCard({
  rule,
  sessionID,
  onToggle,
  onDelete,
}: {
  rule: PermissionRule;
  sessionID: string;
  onToggle: () => void;
  onDelete: () => void;
}) {
  return (
    <div className={`p-4 rounded-xl border transition-all ${
      rule.Enabled
        ? "bg-base-subtle border-green/30"
        : "bg-base-overlay border-surface opacity-60"
    }`}>
      <div className="flex items-start justify-between gap-3">
        <div className="flex items-start gap-3 flex-1 min-w-0">
          {/* Toggle */}
          <button
            onClick={onToggle}
            className={`mt-0.5 w-5 h-5 rounded border-2 flex items-center justify-center transition-colors flex-shrink-0 ${
              rule.Enabled
                ? "bg-green border-green text-white"
                : "border-surface hover:border-green/50"
            }`}
          >
            {rule.Enabled && <Check size={14} />}
          </button>

          {/* Content */}
          <div className="flex-1 min-w-0">
            <div className="flex items-center gap-2 mb-1">
              <span className="font-mono font-semibold text-mauve">{rule.ToolName}</span>
              <span className="text-xs font-mono bg-green/20 text-green rounded px-1.5 py-0.5">
                {rule.Action}
              </span>
              {!rule.Enabled && (
                <span className="text-xs text-text-subtle">(Disabled)</span>
              )}
            </div>
            {rule.Path && (
              <p className="text-sm text-text-subtle font-mono truncate">{rule.Path}</p>
            )}
            <p className="text-xs text-text-subtle mt-1">
              Created {new Date(rule.CreatedAt * 1000).toLocaleDateString()}
            </p>
          </div>
        </div>

        {/* Delete */}
        <button
          onClick={onDelete}
          className="text-text-subtle hover:text-red transition-colors p-1 rounded hover:bg-red/10 flex-shrink-0"
          title="Delete rule"
        >
          <Trash2 size={16} />
        </button>
      </div>
    </div>
  );
}
