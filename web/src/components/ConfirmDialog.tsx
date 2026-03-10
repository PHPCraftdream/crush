import { useEffect } from "react";
import { AlertTriangle, Trash2, X } from "lucide-react";

interface Props {
  title: string;
  message: string;
  confirmLabel?: string;
  /** "danger" = red button (default), "warning" = yellow */
  variant?: "danger" | "warning";
  onConfirm: () => void;
  onCancel: () => void;
}

export function ConfirmDialog({
  title,
  message,
  confirmLabel = "Delete",
  variant = "danger",
  onConfirm,
  onCancel,
}: Props) {
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onCancel();
      if (e.key === "Enter") { e.preventDefault(); onConfirm(); }
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onConfirm, onCancel]);

  const isDanger = variant === "danger";

  return (
    <div
      className="fixed inset-0 z-[100] flex items-center justify-center bg-black/40 backdrop-blur-sm p-4"
      onClick={onCancel}
    >
      <div
        className="bg-canvas border border-surface rounded-2xl shadow-2xl w-full max-w-sm overflow-hidden chat-font"
        onClick={(e) => e.stopPropagation()}
      >
        {/* icon strip */}
        <div className={`px-6 pt-6 pb-4 flex items-start gap-4`}>
          <div className={`shrink-0 w-10 h-10 rounded-xl flex items-center justify-center ${
            isDanger ? "bg-red/10 text-red" : "bg-yellow/10 text-yellow"
          }`}>
            {isDanger ? <Trash2 size={20} /> : <AlertTriangle size={20} />}
          </div>
          <div className="flex-1 min-w-0">
            <h3 className="text-[15px] font-semibold text-text leading-snug">{title}</h3>
            <p className="text-sm text-text-muted mt-1 leading-relaxed">{message}</p>
          </div>
          <button
            onClick={onCancel}
            className="shrink-0 p-1 rounded-lg text-text-subtle hover:text-text hover:bg-base-overlay transition-colors"
          >
            <X size={16} />
          </button>
        </div>

        {/* actions */}
        <div className="flex items-center justify-end gap-2 px-6 py-4 border-t border-surface bg-base-subtle">
          <button
            onClick={onCancel}
            className="px-4 py-2 text-sm font-medium text-text-muted hover:text-text hover:bg-base-overlay rounded-xl transition-colors"
          >
            Cancel
          </button>
          <button
            onClick={onConfirm}
            className={`px-4 py-2 text-sm font-semibold text-white/90 rounded-xl transition-all active:scale-[0.97] shadow-sm ${
              isDanger
                ? "bg-red-fill hover:opacity-90 shadow-red/20"
                : "bg-yellow-fill hover:opacity-90 shadow-yellow/20"
            }`}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
