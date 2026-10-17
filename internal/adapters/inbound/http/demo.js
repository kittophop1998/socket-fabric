// Generated from demo.ts by demo-build.mjs; do not edit.
// Demo only: the server authorizes every new connection and room operation.
                                                                                    
                                                      
                                                                                                   
                                                                           
                     
              
                      
                                  
                                                           
                                 
                       
                                                
                                 
                                
  

const maxRooms = 128;
const maxDelay = 30_000;

export function reconnectDelay(attempt        , random        , overloaded = false)         {
  const base = overloaded ? 2_000 : 500;
  const ceiling = Math.min(maxDelay, base * 2 ** Math.min(Math.max(0, attempt), 16));
  return Math.round(ceiling * (0.5 + 0.5 * Math.min(1, Math.max(0, random))));
}

export class ReconnectingClient {
                   desiredRooms = new Set        ();
                   sentRooms = new Set        ();
          socket         ;
          retry        ;
          stable        ;
          enabled = false;
          authenticated = false;
          terminal = false;
          generation = 0;
          attempt = 0;
          rejectedToken         ;
          message = "Disconnected";
                   dependencies              ;

  constructor(dependencies              ) {
    this.dependencies = dependencies;
  }

  connect(roomId        )       {
    const credential = this.dependencies.token().trim();
    if (!credential) {
      this.dependencies.log("Paste a client token before connecting.");
      return;
    }
    if (credential === this.rejectedToken) {
      this.dependencies.log("Paste a fresh token before reconnecting.");
      return;
    }
    this.terminal = false;
    this.rejectedToken = undefined;
    this.enabled = true;
    this.authenticated = false;
    this.attempt = 0;
    this.generation++;
    this.cancelTimers();
    if (roomId.trim()) this.rememberRoom(roomId.trim());
    this.sentRooms.clear();
    // A replacement starts only after the previous socket has actually closed.
    if (this.socket && this.socket.readyState !== 3) {
      this.update("Closing previous connection…");
      this.closeSocket();
      return;
    }
    this.socket = undefined;
    this.open();
  }

  disconnect()       {
    this.enabled = false;
    this.terminal = false;
    this.authenticated = false;
    this.generation++;
    this.cancelTimers();
    this.desiredRooms.clear();
    this.sentRooms.clear();
    this.closeSocket();
    this.update(this.socket && this.socket.readyState !== 3 ? "Disconnecting…" : "Disconnected");
  }

  join(roomId        )       {
    roomId = roomId.trim();
    if (!roomId) {
      this.dependencies.log("Enter a room ID.");
      return;
    }
    this.rememberRoom(roomId);
    this.reconcileRooms();
  }

  leave(roomId        )       {
    this.desiredRooms.delete(roomId.trim());
    this.reconcileRooms();
  }

          rememberRoom(roomId        )       {
    if (roomId.length > 128) {
      this.dependencies.log("Room IDs must contain at most 128 characters.");
      return;
    }
    if (this.desiredRooms.size >= maxRooms && !this.desiredRooms.has(roomId)) {
      this.dependencies.log("Demo subscription limit reached; leave a room first.");
      return;
    }
    this.desiredRooms.add(roomId);
  }

          open()       {
    if (!this.enabled || this.socket) return;
    const credential = this.dependencies.token().trim();
    if (!credential || credential === this.rejectedToken) {
      this.rejectCredential(credential);
      return;
    }
    const generation = this.generation;
    let connection        ;
    try {
      connection = this.dependencies.socket(this.dependencies.url);
    } catch {
      this.scheduleRetry(false);
      return;
    }
    this.socket = connection;
    this.update("Connecting…");
    const current = () => this.enabled && generation === this.generation && this.socket === connection;
    connection.addEventListener("open", () => {
      if (!current()) return;
      this.update("Authenticating…");
      this.send({ op: "auth", token: credential });
    });
    connection.addEventListener("message", message => {
      if (!current()) return;
      let frame       ;
      try {
        if (typeof message.data !== "string" || message.data.length > 256 * 1024) throw new Error();
        frame = JSON.parse(message.data);
        if (!frame || Array.isArray(frame) || typeof frame !== "object") throw new Error();
      } catch {
        this.dependencies.log("Received an invalid server message.");
        return;
      }
      this.dependencies.frame(frame);
      if (frame.op === "authenticated" && !this.authenticated) {
        this.authenticated = true;
        this.update("Authenticated as " + frame.userId + " in " + frame.projectId);
        const timer = this.dependencies.schedule(() => {
          if (!current() || this.stable !== timer) return;
          this.stable = undefined;
          if (this.authenticated) this.attempt = 0;
        }, 30_000);
        this.stable = timer;
        this.reconcileRooms();
      } else if (frame.op === "joined") {
        this.update("Subscribed to " + frame.roomId);
      } else if (frame.op === "left") {
        this.update("Left " + frame.roomId);
      } else if (frame.op === "error" && frame.error?.code === "unauthorized") {
        this.rejectCredential(credential);
      }
    });
    connection.addEventListener("error", () => {
      if (current()) this.dependencies.log("Connection error. Check the server and origin configuration.");
    });
    connection.addEventListener("close", event => {
      if (this.socket !== connection) return;
      this.socket = undefined;
      this.authenticated = false;
      this.sentRooms.clear();
      this.cancelStable();
      if (generation !== this.generation) {
        if (this.enabled) this.open();
        else this.update(this.terminal ? this.message : "Disconnected");
        return;
      }
      this.dependencies.log("Connection closed (code " + event.code + ").");
      if (event.code === 1008) {
        this.rejectCredential(credential);
      } else if (this.enabled) {
        this.scheduleRetry(event.code === 1013);
      } else if (!this.terminal) {
        this.update("Disconnected (code " + event.code + ")");
      }
    });
  }

          reconcileRooms()       {
    if (!this.authenticated) return;
    for (const roomId of this.sentRooms) {
      if (!this.desiredRooms.has(roomId) && this.send({ op: "leave", roomId })) this.sentRooms.delete(roomId);
    }
    for (const roomId of this.desiredRooms) {
      if (!this.sentRooms.has(roomId) && this.send({ op: "join", roomId })) this.sentRooms.add(roomId);
    }
  }

          send(command                        )          {
    if (!this.socket || this.socket.readyState !== 1 || !this.enabled) return false;
    try {
      this.socket.send(JSON.stringify(command));
      return true;
    } catch {
      this.closeSocket();
      return false;
    }
  }

          scheduleRetry(overloaded         )       {
    if (!this.enabled || this.retry !== undefined) return;
    const delay = reconnectDelay(this.attempt++, this.dependencies.random(), overloaded);
    const generation = this.generation;
    const timer = this.dependencies.schedule(() => {
      if (generation !== this.generation || this.retry !== timer) return;
      this.retry = undefined;
      if (this.enabled) this.open();
    }, delay);
    this.retry = timer;
    this.update("Reconnecting in " + (delay / 1000).toFixed(1) + "s…");
  }

          rejectCredential(credential        )       {
    this.rejectedToken = credential;
    this.enabled = false;
    this.terminal = true;
    this.authenticated = false;
    this.generation++;
    this.cancelTimers();
    // The server follows an unauthorized frame with its policy close (1008).
    // Let that close arrive so a client-initiated 1000 does not replace it.
    this.update("Authentication expired or rejected. Paste a fresh token.");
  }

          closeSocket()       {
    if (!this.socket || this.socket.readyState >= 2) return;
    try { this.socket.close(1000); } catch { /* The browser will report the transport close. */ }
  }

          cancelStable()       {
    if (this.stable !== undefined) this.dependencies.cancel(this.stable);
    this.stable = undefined;
  }

          cancelTimers()       {
    if (this.retry !== undefined) this.dependencies.cancel(this.retry);
    this.retry = undefined;
    this.cancelStable();
  }

          update(message        )       {
    this.message = message;
    this.dependencies.status(message, {
      active: Boolean(this.socket && this.socket.readyState !== 3),
      authenticated: this.authenticated,
      retrying: this.retry !== undefined,
    });
  }
}

// No tokens enter a URL, browser storage, console, or outbound log message.
if (typeof document !== "undefined") {
  const clients                       = [];
  document.querySelectorAll             ("[data-client]").forEach(panel => {
    const token = panel.querySelector                     ("[data-token]") ;
    const room = panel.querySelector                  ("[data-room]") ;
    const status = panel.querySelector             ("[data-status]") ;
    const log = panel.querySelector             ("[data-log]") ;
    const connect = panel.querySelector                   ("[data-connect]") ;
    const join = panel.querySelector                   ("[data-join]") ;
    const leave = panel.querySelector                   ("[data-leave]") ;
    const disconnect = panel.querySelector                   ("[data-disconnect]") ;
    const append = (message        ) => {
      const line = "[" + new Date().toLocaleTimeString() + "] " + message + "\n";
      log.textContent = ((log.textContent ?? "") + line).slice(-24_000);
      log.scrollTop = log.scrollHeight;
    };
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    const client = new ReconnectingClient({
      url: scheme + "//" + location.host + "/ws",
      token: () => token.value,
      socket: url => new WebSocket(url),
      schedule: (callback, delay) => globalThis.setTimeout(callback, delay),
      cancel: timer => globalThis.clearTimeout(timer),
      random: Math.random,
      status: (message, view) => {
        status.textContent = message;
        connect.disabled = view.active || view.retrying;
        disconnect.disabled = !view.active && !view.retrying;
        join.disabled = !view.authenticated;
        leave.disabled = !view.authenticated;
      },
      log: append,
      frame: frame => append(JSON.stringify(frame, null, 2)),
    });
    clients.push(client);
    connect.addEventListener("click", () => client.connect(room.value));
    join.addEventListener("click", () => client.join(room.value));
    leave.addEventListener("click", () => client.leave(room.value));
    disconnect.addEventListener("click", () => client.disconnect());
    panel.querySelector("[data-clear]") .addEventListener("click", () => { log.textContent = ""; });
    token.value = "";
  });
  window.addEventListener("pagehide", () => {
    clients.forEach(client => client.disconnect());
    document.querySelectorAll                     ("[data-token]").forEach(input => { input.value = ""; });
  });
}
