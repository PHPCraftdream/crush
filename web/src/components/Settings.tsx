import { useStore } from "@nanostores/react";
import { $config, $activeSessionID, $sessionLargeModel, $sessionSmallModel, setSessionLargeModel, setSessionSmallModel } from "../store";

export function Settings({ onClose }: { onClose: () => void }) {
  const config = useStore($config);
  const activeSessionID = useStore($activeSessionID);
  const sessionLargeModels = useStore($sessionLargeModel);
  const sessionSmallModels = useStore($sessionSmallModel);

  const models = config?.models ?? {};
  const providers = config?.providers ?? {};
  const modelEntries = Object.entries(models);

  const currentLargeKey = activeSessionID ? (sessionLargeModels[activeSessionID] ?? "large") : "large";
  const currentSmallKey = activeSessionID ? (sessionSmallModels[activeSessionID] ?? "small") : "small";

  return (
    <>
      {/* Backdrop */}
      <div className="fixed inset-0 z-40 bg-black/20 backdrop-blur-sm" onClick={onClose} />

      {/* Panel */}
      <div className="fixed top-0 right-0 h-full w-96 max-w-[90vw] bg-white border-l border-surface z-50 flex flex-col shadow-xl">
        <div className="flex items-center justify-between px-6 py-5 border-b border-surface">
          <h2 className="text-lg font-semibold text-text">Settings</h2>
          <button
            onClick={onClose}
            className="w-8 h-8 flex items-center justify-center rounded-lg border border-surface bg-base-overlay text-text-muted hover:text-text hover:border-surface transition-colors text-lg"
          >
            ✕
          </button>
        </div>

        <div className="flex-1 overflow-y-auto p-6 flex flex-col gap-6">

          {/* Theme */}
          <section>
            <h3 className="text-xs font-semibold text-text-subtle uppercase tracking-widest mb-3">
              Theme
            </h3>
            <div className="flex items-center gap-2 bg-base-overlay border border-surface rounded-xl px-4 py-3 text-sm text-text">
              <span className="w-3 h-3 rounded-full bg-accent inline-block" />
              Catppuccin Latte (Light)
            </div>
          </section>

          {/* Session Model Overrides */}
          {activeSessionID && modelEntries.length > 0 && (
            <section>
              <h3 className="text-xs font-semibold text-text-subtle uppercase tracking-widest mb-3">
                Session Models
              </h3>
              <div className="flex flex-col gap-3">
                <div>
                  <label className="text-sm text-text-muted mb-1.5 block font-medium">Large model</label>
                  <select
                    value={currentLargeKey}
                    onChange={e => setSessionLargeModel(activeSessionID, e.target.value)}
                    className="w-full bg-base-overlay border border-surface rounded-lg px-3 py-2.5 text-sm text-text outline-none focus:border-accent transition-colors"
                  >
                    {modelEntries.map(([key, m]) => (
                      <option key={key} value={key}>
                        {m.Model} ({key})
                      </option>
                    ))}
                  </select>
                </div>
                <div>
                  <label className="text-sm text-text-muted mb-1.5 block font-medium">Small model</label>
                  <select
                    value={currentSmallKey}
                    onChange={e => setSessionSmallModel(activeSessionID, e.target.value)}
                    className="w-full bg-base-overlay border border-surface rounded-lg px-3 py-2.5 text-sm text-text outline-none focus:border-accent transition-colors"
                  >
                    {modelEntries.map(([key, m]) => (
                      <option key={key} value={key}>
                        {m.Model} ({key})
                      </option>
                    ))}
                  </select>
                </div>
              </div>
            </section>
          )}

          {/* Configured Models */}
          {modelEntries.length > 0 && (
            <section>
              <h3 className="text-xs font-semibold text-text-subtle uppercase tracking-widest mb-3">
                Active Models
              </h3>
              <div className="flex flex-col gap-2">
                {modelEntries.map(([role, m]) => (
                  <div key={role} className="bg-base-overlay border border-surface rounded-xl px-4 py-3">
                    <div className="text-xs font-medium text-text-subtle uppercase tracking-wider mb-1">{role}</div>
                    <div className="text-sm text-text font-medium truncate">{m.Model}</div>
                    <div className="text-xs text-text-muted mt-0.5">{m.Provider}</div>
                  </div>
                ))}
              </div>
            </section>
          )}

          {/* Providers */}
          {Object.keys(providers).length > 0 && (
            <section>
              <h3 className="text-xs font-semibold text-text-subtle uppercase tracking-widest mb-3">
                Providers
              </h3>
              <div className="flex flex-col gap-2">
                {Object.entries(providers).map(([name, p]) => (
                  <div key={name} className="bg-base-overlay border border-surface rounded-xl px-4 py-3">
                    <div className="flex items-center justify-between">
                      <div className="text-sm font-semibold text-text">{name}</div>
                      {p.models && p.models.length > 0 && (
                        <span className="text-xs text-text-subtle bg-base-subtle border border-surface rounded-full px-2 py-0.5">
                          {p.models.length} model{p.models.length !== 1 ? "s" : ""}
                        </span>
                      )}
                    </div>
                    {p.models && p.models.length > 0 && (
                      <div className="mt-2 flex flex-col gap-0.5">
                        {p.models.slice(0, 5).map(m => (
                          <div key={m.id} className="text-xs text-text-muted font-mono truncate">{m.id}</div>
                        ))}
                        {p.models.length > 5 && (
                          <div className="text-xs text-text-subtle">+{p.models.length - 5} more</div>
                        )}
                      </div>
                    )}
                  </div>
                ))}
              </div>
            </section>
          )}

          {!config && (
            <div className="flex flex-col items-center justify-center py-12 text-center">
              <div className="w-8 h-8 border-2 border-accent border-t-transparent rounded-full animate-spin mb-3" />
              <p className="text-text-subtle text-sm">Loading configuration…</p>
            </div>
          )}
        </div>
      </div>
    </>
  );
}
