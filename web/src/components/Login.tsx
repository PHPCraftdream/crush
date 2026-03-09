import { useState } from "react";
import { $authed } from "../store";

export function Login() {
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setLoading(true);
    setError("");
    try {
      const res = await fetch("/auth", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token }),
      });
      if (res.ok) {
        $authed.set(true);
      } else {
        setError("Invalid token");
      }
    } catch {
      setError("Connection error");
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="fixed inset-0 flex items-center justify-center bg-white">
      <div className="w-96 max-w-[90vw] bg-base-subtle border border-surface rounded-lg p-8">
        <h2 className="text-2xl font-bold text-accent mb-1">crush web</h2>
        <p className="text-text-subtle text-sm mb-6">
          Enter the token shown in your terminal to continue.
        </p>
        <form onSubmit={submit} className="flex flex-col gap-3">
          <input
            type="password"
            placeholder="Access token"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            autoFocus
            className="bg-base-overlay border border-surface rounded px-3 py-2 text-text outline-none focus:border-accent transition-colors"
          />
          {error && <p className="text-red text-sm">{error}</p>}
          <button
            type="submit"
            disabled={loading || !token}
            className="bg-accent text-base font-semibold rounded py-2 disabled:opacity-40 hover:opacity-90 transition-opacity"
          >
            {loading ? "Verifying…" : "Connect"}
          </button>
        </form>
      </div>
    </div>
  );
}
