import { useEffect } from "react";
import { useStore } from "@nanostores/react";
import { $authed, $connected } from "./store";
import { useWS } from "./useWS";
import { Sidebar } from "./components/Sidebar";
import { Chat } from "./components/Chat";
import { Header } from "./components/Header";
import { StatusBar } from "./components/StatusBar";
import { Login } from "./components/Login";

function AuthedApp() {
  useWS();
  const connected = useStore($connected);

  return (
    <div className="flex h-full overflow-hidden bg-canvas">
      {!connected && (
        <div className="fixed top-0 inset-x-0 bg-yellow text-base-subtle text-center py-1 text-sm font-semibold z-50">
          Reconnecting…
        </div>
      )}
      <Sidebar />
      <div className="flex-1 flex flex-col overflow-hidden">
        <Header />
        <Chat />
        <StatusBar />
      </div>
    </div>
  );
}

export function App() {
  const authed = useStore($authed);

  useEffect(() => {
    fetch("/auth/check").then((r) => {
      if (r.ok) $authed.set(true);
    });
  }, []);

  if (!authed) return <Login />;
  return <AuthedApp />;
}
