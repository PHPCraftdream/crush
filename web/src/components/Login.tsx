import { useState } from "react";
import { KeyRound, Loader2, Terminal, Sun, Moon } from "lucide-react";
import { $authed, setTheme } from "../store";

export function Login() {
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [isDark, setIsDark] = useState(() =>
    document.documentElement.classList.contains("dark")
  );

  function toggleTheme() {
    const next = isDark ? "light" : "dark";
    setTheme(next);
    setIsDark(!isDark);
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setLoading(true);
    setError("");
    try {
      const res = await fetch("/auth", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token: token.trim() }),
      });
      if (res.ok) {
        $authed.set(true);
      } else {
        setError("Invalid token. Check your terminal and try again.");
      }
    } catch {
      setError("Connection error. Is the server running?");
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="fixed inset-0 flex items-center justify-center bg-base-subtle">
      {/* theme toggle — top right */}
      <button
        onClick={toggleTheme}
        title={isDark ? "Switch to light theme" : "Switch to dark theme"}
        className="absolute top-4 right-4 flex items-center justify-center w-8 h-8 rounded-lg border transition-colors bg-canvas border-surface text-text-subtle hover:border-accent/50 hover:text-text z-10"
      >
        {isDark ? <Sun size={14} /> : <Moon size={14} />}
      </button>

      <div className="relative w-full max-w-sm mx-4">
        {/* card */}
        <div className="bg-canvas border border-surface rounded-2xl shadow-xl overflow-hidden">
          {/* header strip */}
          <div className="bg-accent-fill px-8 pt-8 pb-7">
            <div className="flex items-center gap-3 mb-4">
              <div className="w-9 h-9 rounded-xl bg-white/15 flex items-center justify-center">
                <Terminal size={18} className="text-white/90" />
              </div>
              <span className="text-white/60 text-sm font-medium tracking-wide uppercase">
                crush
              </span>
            </div>
            <h1 className="text-2xl font-bold text-white/90 leading-tight">
              Welcome back
            </h1>
            <p className="text-white/55 text-sm mt-1">
              Paste the token from your terminal to continue.
            </p>
          </div>

          {/* form body */}
          <div className="px-8 py-7">
            <form onSubmit={submit} className="flex flex-col gap-4">
              <div className="flex flex-col gap-1.5">
                <label className="text-xs font-semibold text-text-muted uppercase tracking-wider">
                  Access token
                </label>
                <div className="relative">
                  <KeyRound
                    size={15}
                    className="absolute left-3.5 top-1/2 -translate-y-1/2 text-text-subtle pointer-events-none"
                  />
                  <input
                    type="password"
                    placeholder="••••••••••••••••"
                    value={token}
                    onChange={(e) => { setToken(e.target.value); setError(""); }}
                    autoFocus
                    autoComplete="off"
                    className="w-full bg-base-subtle border border-surface rounded-xl pl-9 pr-4 py-2.5 text-sm text-text placeholder:text-text-subtle outline-none focus:border-accent focus:ring-2 focus:ring-accent/10 transition-all"
                  />
                </div>
                {error && (
                  <p className="text-red text-xs mt-0.5 flex items-center gap-1">
                    <span className="font-bold">⚠</span> {error}
                  </p>
                )}
              </div>

              <button
                type="submit"
                disabled={loading || !token.trim()}
                className="flex items-center justify-center gap-2 w-full bg-accent-fill hover:opacity-90 active:scale-[0.98] disabled:opacity-40 disabled:pointer-events-none text-white/90 font-semibold text-sm rounded-xl py-2.5 transition-all shadow-md"
              >
                {loading ? (
                  <>
                    <Loader2 size={15} className="animate-spin" />
                    Verifying…
                  </>
                ) : (
                  "Connect"
                )}
              </button>
            </form>
          </div>
        </div>

        <p className="text-center text-xs text-text-subtle mt-4">
          Token is shown once at server startup.
        </p>
      </div>
    </div>
  );
}
