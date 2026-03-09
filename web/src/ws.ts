import type { WSMessage } from "./types";

type Handler = (msg: WSMessage) => void;

class WSClient {
  private socket: WebSocket | null = null;
  private handlers = new Map<string, Set<Handler>>();
  private reconnectDelay = 1000;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private closed = false;

  connect() {
    this.closed = false;
    this._connect();
  }

  private _connect() {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${proto}//${location.host}/ws`;
    const sock = new WebSocket(url);
    this.socket = sock;

    sock.onopen = () => {
      this.reconnectDelay = 1000;
      this.emit("_connected", { type: "_connected" });
    };

    sock.onmessage = (ev: MessageEvent<string>) => {
      try {
        const msg: WSMessage = JSON.parse(ev.data);
        this.emit(msg.type, msg);
        this.emit("*", msg);
      } catch {
        // ignore malformed
      }
    };

    sock.onclose = () => {
      this.socket = null;
      this.emit("_disconnected", { type: "_disconnected" });
      if (!this.closed) {
        this.reconnectTimer = setTimeout(() => {
          this.reconnectDelay = Math.min(this.reconnectDelay * 2, 30000);
          this._connect();
        }, this.reconnectDelay);
      }
    };

    sock.onerror = () => {
      sock.close();
    };
  }

  disconnect() {
    this.closed = true;
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    this.socket?.close();
    this.socket = null;
  }

  send<T>(type: string, payload?: T, id?: string) {
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN) return;
    const msg: WSMessage<T> = { type, payload, id };
    this.socket.send(JSON.stringify(msg));
  }

  on(type: string, handler: Handler): () => void {
    if (!this.handlers.has(type)) this.handlers.set(type, new Set());
    this.handlers.get(type)!.add(handler);
    return () => this.handlers.get(type)?.delete(handler);
  }

  private emit(type: string, msg: WSMessage) {
    this.handlers.get(type)?.forEach((h) => h(msg));
  }
}

export const ws = new WSClient();
