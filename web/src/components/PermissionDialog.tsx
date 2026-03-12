import { createPortal } from "react-dom";
import { useStore } from "@nanostores/react";
import { useState } from "react";
import { $permissions, $activeSessionID, removePermission, setYolo } from "../store";
import { ws } from "../ws";
import type { PermissionRequest } from "../types";
import { Zap, Settings } from "lucide-react";
import { PermissionsModal } from "./PermissionsModal";

export function PermissionDialog() {
  const permissions = useStore($permissions);
  const [showModal, setShowModal] = useState(false);

  if (permissions.length === 0 && !showModal) return null;

  return createPortal(
    <>
      {/* Manage Permissions Button - always visible when there's an active session */}
      {permissions.length === 0 && (
        <button
          onClick={() => setShowModal(true)}
          className="fixed bottom-6 right-6 z-[9998] flex items-center gap-2 bg-base-subtle border border-surface text-text-subtle hover:text-text rounded-xl px-4 py-2 shadow-lg transition-colors"
          title="Manage permissions"
        >
          <Settings size={16} />
          <span className="text-sm font-medium">Permissions</span>
        </button>
      )}

      {/* Permission Requests */}
      {permissions.length > 0 && (
        <div className="fixed bottom-24 left-1/2 -translate-x-1/2 w-[min(600px,90vw)] flex flex-col gap-2 z-[9999]">
          {permissions.map((p) => (
            <PermissionCard key={p.ToolCallID} perm={p} onOpenSettings={() => setShowModal(true)} />
          ))}
        </div>
      )}

      {/* Permissions Modal */}
      {showModal && <PermissionsModal onClose={() => setShowModal(false)} />}
    </>,
    document.body
  );
}

function PermissionCard({ perm, onOpenSettings }: { perm: PermissionRequest; onOpenSettings?: () => void }) {
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
      setYolo(true);
    }
    grant(false);
  }

  return (
    <div className="bg-base-subtle border border-yellow rounded-lg p-4 shadow-xl">
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
        {onOpenSettings && (
          <button
            onClick={onOpenSettings}
            className="bg-base-overlay border border-surface text-text-subtle rounded px-3 py-1 text-sm hover:bg-canvas transition-colors"
            title="Manage all permissions"
          >
            <Settings size={14} />
          </button>
        )}
        <button
          onClick={deny}
          className="bg-base-overlay border border-surface text-red rounded px-3 py-1 text-sm hover:bg-canvas transition-colors"
        >
          Deny
        </button>
        <button
          onClick={() => grant(false)}
          className="bg-base-overlay border border-surface text-green rounded px-3 py-1 text-sm hover:bg-canvas transition-colors"
        >
          Allow
        </button>
        <button
          onClick={() => grant(true)}
          className="bg-green text-base font-semibold rounded px-3 py-1 text-sm hover:opacity-90 transition-opacity"
        >
          Allow always
        </button>
        <button
          onClick={goYolo}
          className="flex items-center gap-1.5 bg-yellow/20 border border-yellow/40 text-yellow font-semibold rounded px-3 py-1 text-sm hover:bg-yellow/30 transition-colors"
          title="Enable Yolo mode — all future permissions auto-approved"
        >
          <Zap size={13} />
          Yolo
        </button>
      </div>
    </div>
  );
}
