import { useStore } from "@nanostores/react";
import { $permissions, removePermission } from "../store";
import { ws } from "../ws";
import type { PermissionRequest } from "../types";

export function PermissionDialog() {
  const permissions = useStore($permissions);
  if (permissions.length === 0) return null;

  return (
    <div className="fixed bottom-20 left-1/2 -translate-x-1/2 w-[min(600px,90vw)] flex flex-col gap-2 z-50">
      {permissions.map((p) => (
        <PermissionCard key={p.ToolCallID} perm={p} />
      ))}
    </div>
  );
}

function PermissionCard({ perm }: { perm: PermissionRequest }) {
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
      </div>
    </div>
  );
}
