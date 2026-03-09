import { useStore } from "@nanostores/react";
import { $lspStates, $mcpState, $connected } from "../store";

function lspDot(state: string): string {
  switch (state) {
    case "running":
    case "ready":
      return "bg-green";
    case "starting":
      return "bg-yellow";
    case "error":
    case "stopped":
      return "bg-red";
    default:
      return "bg-surface";
  }
}

function mcpDot(status: string): string {
  switch (status) {
    case "connected":
    case "running":
      return "bg-green";
    case "connecting":
      return "bg-yellow";
    case "error":
    case "disconnected":
      return "bg-red";
    default:
      return "bg-surface";
  }
}

export function StatusBar() {
  const lspStates = useStore($lspStates);
  const mcpState = useStore($mcpState);
  const connected = useStore($connected);

  return (
    <div className="flex items-center gap-4 px-5 py-2 border-t border-surface bg-base-subtle text-xs text-text-subtle shrink-0 overflow-x-auto">
      {/* Connection */}
      <div className="flex items-center gap-1.5 shrink-0">
        <span
          className={`w-1.5 h-1.5 rounded-full inline-block ${connected ? "bg-green" : "bg-red"}`}
        />
        <span>{connected ? "Connected" : "Disconnected"}</span>
      </div>

      {/* LSP */}
      {lspStates.length > 0 && (
        <>
          <span className="text-surface">|</span>
          <div className="flex items-center gap-3 shrink-0">
            <span className="text-text-subtle font-semibold">LSP</span>
            {lspStates.map((l) => (
              <div key={l.name} className="flex items-center gap-1" title={`${l.name}: ${l.state}`}>
                <span className={`w-1.5 h-1.5 rounded-full inline-block ${lspDot(l.state)}`} />
                <span>{l.name}</span>
                {l.diagnosticCount > 0 && (
                  <span className="text-yellow ml-0.5">({l.diagnosticCount})</span>
                )}
              </div>
            ))}
          </div>
        </>
      )}

      {/* MCP */}
      {mcpState && (mcpState.servers?.length ?? 0) > 0 && (
        <>
          <span className="text-surface">|</span>
          <div className="flex items-center gap-3 shrink-0">
            <span className="text-text-subtle font-semibold">MCP</span>
            {(mcpState.servers ?? []).map((srv) => (
              <div
                key={srv.name}
                className="flex items-center gap-1"
                title={`${srv.name}: ${srv.status}`}
              >
                <span className={`w-1.5 h-1.5 rounded-full inline-block ${mcpDot(srv.status)}`} />
                <span>{srv.name}</span>
              </div>
            ))}
          </div>
        </>
      )}
    </div>
  );
}
