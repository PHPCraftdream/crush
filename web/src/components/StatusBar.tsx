import { useStore } from "@nanostores/react";
import { $mcpState, $connected } from "../store";

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
  const mcpState = useStore($mcpState);
  const connected = useStore($connected);

  return (
    <div data-test-id="status-bar" className="flex items-center gap-4 px-5 py-2 border-t border-surface bg-base-subtle text-xs text-text-subtle shrink-0 overflow-x-auto">
      {/* Connection */}
      <div data-test-id="status-connection" className="flex items-center gap-1.5 shrink-0">
        <span
          className={`w-1.5 h-1.5 rounded-full inline-block ${connected ? "bg-green" : "bg-red"}`}
        />
        <span>{connected ? "Connected" : "Disconnected"}</span>
      </div>

      {/* MCP */}
      {mcpState && (mcpState.servers?.length ?? 0) > 0 && (
        <>
          <span className="text-surface">|</span>
          <div data-test-id="status-mcp" className="flex items-center gap-3 shrink-0">
            <span className="text-text-subtle font-semibold">MCP</span>
            {(mcpState.servers ?? []).map((srv) => (
              <div
                key={srv.name}
                data-test-id={`status-mcp-${srv.name}`}
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
