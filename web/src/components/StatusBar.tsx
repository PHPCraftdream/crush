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

// StatusBar shows the WebSocket connection state and MCP server health.
// `inline` switches off the standalone strip styling (border, background,
// extra padding) so the same widget can sit in the middle of ChatToolbar
// without breaking that row's flex layout. Default behaviour matches the
// original standalone footer strip.
export function StatusBar({ inline = false }: { inline?: boolean }) {
  const mcpState = useStore($mcpState);
  const connected = useStore($connected);

  const outerClass = inline
    ? "flex items-center gap-3 text-xs text-text-subtle shrink-0 min-w-0 overflow-x-auto"
    : "flex items-center gap-4 px-5 py-2 border-t border-surface bg-base-subtle text-xs text-text-subtle shrink-0 overflow-x-auto";

  return (
    <div data-test-id="status-bar" className={outerClass}>
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
