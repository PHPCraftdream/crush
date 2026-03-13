import { createPortal } from "react-dom";
import { useStore } from "@nanostores/react";
import { $permissions, $activeSessionID, removePermission, setYolo } from "../store";
import { ws } from "../ws";
import type { PermissionRequest } from "../types";
import { Zap } from "lucide-react";

export function PermissionDialog() {
  const permissions = useStore($permissions);

  if (permissions.length === 0) return null;

  return createPortal(
    <>
      {/* Permission Requests */}
      {permissions.length > 0 && (
        <div className="fixed bottom-24 left-1/2 -translate-x-1/2 w-[min(600px,90vw)] flex flex-col gap-2 z-[9999]" data-test-id="permission-dialog-container">
          {permissions.map((p) => (
            <PermissionCard key={p.ToolCallID} perm={p} />
          ))}
        </div>
      )}
    </>,
    document.body
  );
}

function PermissionCard({ perm }: { perm: PermissionRequest }) {
  const activeSessionID = useStore($activeSessionID);

  function grant(persistent: boolean) {
    ws.send(persistent ? "grant_permission_persistent" : "grant_permission", {
      permissionID: perm.ID,
    });
    removePermission(perm.ToolCallID);
  }

  function deny() {
    ws.send("deny_permission", { permissionID: perm.ID });
    removePermission(perm.ToolCallID);
  }

  function goYolo() {
    // Enable yolo for this session — grants current request and auto-approves all future ones
    if (activeSessionID) {
      setYolo(activeSessionID, true);
    }
    grant(false);
  }

  return (
    <div className="bg-base-subtle border border-yellow rounded-lg p-4 shadow-xl" data-test-id={`permission-${perm.ToolCallID}`}>
      <div className="flex items-center gap-2 mb-2">
        <span className="text-mauve font-mono font-bold">{perm.ToolName}</span>
        <span className="text-yellow font-mono text-xs bg-base-overlay rounded px-1.5 py-0.5">
          {perm.Action}
        </span>
      </div>
      <p className="text-text-muted text-sm mb-2">{perm.Description}</p>
      {perm.Path && (
        <p className="text-text-subtle font-mono text-xs mb-3 break-all">
          {perm.Path}
        </p>
      )}
      <div className="flex gap-2 justify-end">
        <button
          onClick={deny}
          className="bg-base-overlay border border-surface text-red rounded px-3 py-1 text-sm hover:bg-canvas transition-colors"
          data-test-id="permission-deny"
        >
          Deny
        </button>
        <button
          onClick={() => grant(false)}
          className="bg-base-overlay border border-surface text-green rounded px-3 py-1 text-sm hover:bg-canvas transition-colors"
          data-test-id="permission-allow"
        >
          Allow
        </button>
        <button
          onClick={() => grant(true)}
          className="bg-green text-base font-semibold rounded px-3 py-1 text-sm hover:opacity-90 transition-opacity"
          data-test-id="permission-allow-always"
        >
          Allow always
        </button>
        <button
          onClick={goYolo}
          className="flex items-center gap-1.5 bg-yellow/20 border border-yellow/40 text-yellow font-semibold rounded px-3 py-1 text-sm hover:bg-yellow/30 transition-colors"
          title="Enable Yolo mode — all future permissions auto-approved"
          data-test-id="permission-yolo"
        >
          <Zap size={13} />
          Yolo
        </button>
      </div>
    </div>
  );
}
