Get Crush's current runtime state: active models, provider, MCP status, skills, hooks, permissions, and disabled tools. No parameters needed.

<usage>
- Shows active models and provider, MCP server status, skills,
  hooks, permissions mode, disabled tools, and key options
- Use when diagnosing why something isn't working (missing diagnostics,
  provider errors, MCP disconnections)
- No parameters needed — always returns the full current state
</usage>

<tips>
- Check [model] for the configured model slots: "large" (smart, top-level
  default) and "small" (fast, cheap work) are always meaningful; "worker"
  (cheap sub-task delegation) and "reviewer" (strongest slot, explicit
  review only) are optional and only appear here when configured — their
  absence just means that slot isn't set, not that anything is broken
- Check [mcp] section for service health
- Check [providers] to see which providers are enabled and available
- Check [skills] to see which skills are available and whether they have been
  loaded this session
- Check [hooks] to see which hook events are configured and whether the
  hook runner is active
- Pair with the crush-config skill to fix configuration issues
</tips>
