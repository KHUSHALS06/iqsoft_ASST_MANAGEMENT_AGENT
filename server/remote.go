package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Remote sessions are short-lived relays between one agent and one or more
// viewers. Enrollment (see /enroll in main.go) is the one-time consent event
// for a device; starting a session here never prompts the end user again,
// which is the whole point for a lab/fleet scenario. What this file *does*
// enforce every time:
//   - only an authenticated admin can create a session for an enrolled device
//   - the agent must show a visible on-screen indicator while a session is
//     open (enforced agent-side, see agent/remote_windows.go) - this file
//     just carries whatever the agent sends, it can't fake that indicator
//   - a session id is single-use: the agent claims it once, and it expires
//     if unclaimed
var (
	remoteMu       sync.Mutex
	remoteSessions = map[string]*remoteSession{}
	upgrader       = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 32 << 10,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
)

const remoteSessionTTL = 2 * time.Minute // must be claimed by the agent within this window

type remoteSession struct {
	id       string
	deviceID string
	hostname string
	mu       sync.Mutex
	agent    *websocket.Conn
	viewers  map[*websocket.Conn]bool
	created  time.Time
}

func newRemoteSession(deviceID, hostname string) *remoteSession {
	s := &remoteSession{
		id:       "rs_" + randHex(8),
		deviceID: deviceID,
		hostname: hostname,
		viewers:  map[*websocket.Conn]bool{},
		created:  time.Now(),
	}
	remoteMu.Lock()
	remoteSessions[s.id] = s
	remoteMu.Unlock()
	time.AfterFunc(remoteSessionTTL, func() {
		s.mu.Lock()
		claimed := s.agent != nil
		s.mu.Unlock()
		if !claimed {
			closeRemoteSession(s.id)
		}
	})
	return s
}

func getRemoteSession(id string) *remoteSession {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	return remoteSessions[id]
}

func closeRemoteSession(id string) {
	remoteMu.Lock()
	s := remoteSessions[id]
	delete(remoteSessions, id)
	remoteMu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.agent != nil {
		s.agent.Close()
	}
	for v := range s.viewers {
		v.Close()
	}
	s.mu.Unlock()
}

func registerRemote() {
	// Admin: start a remote session against an enrolled device. This queues
	// a "start_remote" job the same way any other job is queued - the agent
	// picks it up on its next heartbeat (every 5s) and dials in.
	http.HandleFunc("POST /admin/devices/{id}/remote", admin(func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.PathValue("id")
		mu.Lock()
		dev := devices[deviceID]
		online := dev != nil && time.Since(dev.LastSeen) < onlineWindow
		mu.Unlock()
		if dev == nil {
			http.Error(w, "no such device", http.StatusNotFound)
			return
		}
		if !online {
			http.Error(w, "device is offline", http.StatusConflict)
			return
		}

		s := newRemoteSession(deviceID, dev.Hostname)

		mu.Lock()
		j := &Job{
			ID: "job_" + randHex(4), DeviceID: deviceID, Type: "start_remote",
			Params: map[string]string{"session_id": s.id}, Status: "pending",
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		jobs[j.ID] = j
		save()
		mu.Unlock()

		log.Printf("remote %s: queued for %s (%s)", s.id, dev.ID, dev.Hostname)
		json.NewEncoder(w).Encode(map[string]string{
			"session_id": s.id,
			"viewer_url": "/remote/view?session=" + s.id,
		})
	}))

	// Agent: claims the session once it has picked up the start_remote job.
	// Auth is the same device bearer token used for /heartbeat etc.
	http.HandleFunc("GET /remote/agent-ws", func(w http.ResponseWriter, r *http.Request) {
		d := authDevice(r)
		if d == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s := getRemoteSession(r.URL.Query().Get("session"))
		if s == nil || s.deviceID != d.ID {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		s.mu.Lock()
		if s.agent != nil { // single-use: already claimed
			s.mu.Unlock()
			http.Error(w, "session already claimed", http.StatusConflict)
			return
		}
		s.mu.Unlock()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.agent = conn
		s.mu.Unlock()
		log.Printf("remote %s: agent connected (%s)", s.id, d.Hostname)

		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			s.mu.Lock()
			for v := range s.viewers {
				v.WriteMessage(mt, data)
			}
			s.mu.Unlock()
		}
		log.Printf("remote %s: agent disconnected", s.id)
		closeRemoteSession(s.id)
	})

	// Operator: watches/controls the session from the dashboard. Auth via
	// ?admin_key= because a browser can't set a custom header on the
	// WebSocket handshake request.
	http.HandleFunc("GET /remote/viewer-ws", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("admin_key")), []byte(adminKey)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s := getRemoteSession(r.URL.Query().Get("session"))
		if s == nil {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.viewers[conn] = true
		s.mu.Unlock()
		log.Printf("remote %s: viewer connected", s.id)

		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			if mt == websocket.TextMessage {
				var ctl struct {
					T string `json:"t"`
				}
				if json.Unmarshal(data, &ctl) == nil && ctl.T == "end" {
					log.Printf("remote %s: ended by viewer", s.id)
					closeRemoteSession(s.id)
					break
				}
			}
			s.mu.Lock()
			agent := s.agent
			s.mu.Unlock()
			if agent != nil {
				agent.WriteMessage(mt, data)
			}
		}
		s.mu.Lock()
		delete(s.viewers, conn)
		s.mu.Unlock()
		log.Printf("remote %s: viewer disconnected", s.id)
	})

	http.HandleFunc("GET /remote/view", func(w http.ResponseWriter, r *http.Request) {
		s := getRemoteSession(r.URL.Query().Get("session"))
		if s == nil {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(remoteViewHTML))
	})
}

const remoteViewHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Remote session</title>
<style>
  html, body { height:100%; }
  body { margin:0; background:#1e1e1e; color:#ddd; font-family:system-ui,sans-serif; display:flex; flex-direction:column; }
  #bar { padding:8px 12px; background:#111; font-size:13px; display:flex; gap:12px; align-items:center; }
  #bar b { color:#4ade80; }
  #bar .sp { flex:1; }
  #bar button { padding:5px 12px; border:0; border-radius:4px; background:#333; color:#eee; cursor:pointer; }
  #bar button:hover { background:#444; }
  #bar button.danger { background:#b91c1c; }
  #bar button.danger:hover { background:#dc2626; }
  #bar button:disabled { opacity:.5; cursor:default; }
  #stage { flex:1; min-height:0; display:flex; align-items:center; justify-content:center; overflow:hidden; }
  img { max-width:100%; max-height:100%; cursor:crosshair; }
  #status { color:#f59e0b; }
  #hot { display:none; position:fixed; top:0; left:0; right:0; height:6px; z-index:11; }
  body.fs #hot { display:block; }
  body.fs #bar { position:fixed; top:0; left:0; right:0; z-index:10; transform:translateY(-100%); transition:transform .15s; }
  body.fs #bar.show { transform:none; }
</style>
</head>
<body>
  <div id="bar">
    <span>Remote session <b id="sid"></b></span>
    <span id="status">connecting…</span>
    <span class="sp"></span>
    <button id="fs">Fullscreen</button>
    <button id="end" class="danger">End session</button>
  </div>
  <div id="hot"></div>
  <div id="stage"><img id="frame"></div>
<script>
const params = new URLSearchParams(location.search);
const sid = params.get("session");
document.getElementById("sid").textContent = sid;
const adminKey = sessionStorage.getItem("adminkey") || prompt("Admin key:");
const proto = location.protocol === "https:" ? "wss" : "ws";
const ws = new WebSocket(proto + "://" + location.host + "/remote/viewer-ws?session=" + encodeURIComponent(sid) + "&admin_key=" + encodeURIComponent(adminKey));
ws.binaryType = "arraybuffer";
const img = document.getElementById("frame");
const status = document.getElementById("status");
const bar = document.getElementById("bar");
const hot = document.getElementById("hot");
const fsBtn = document.getElementById("fs");
const endBtn = document.getElementById("end");
let ended = false;

function finish(text) {
  status.textContent = text;
  status.style.color = "#f87171";
  endBtn.disabled = true;
  if (document.fullscreenElement) document.exitFullscreen();
}

ws.onopen = () => status.textContent = "waiting for agent…";
ws.onclose = () => finish(ended ? "session ended" : "disconnected");
ws.onerror = () => { if (!ended) status.textContent = "error"; };

ws.onmessage = (ev) => {
  if (typeof ev.data === "string") return;
  status.textContent = "live";
  status.style.color = "";
  const blob = new Blob([ev.data], { type: "image/jpeg" });
  const url = URL.createObjectURL(blob);
  const old = img.src;
  img.onload = () => old && URL.revokeObjectURL(old);
  img.src = url;
};

function sendEvent(obj) {
  if (!ended && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}

fsBtn.addEventListener("click", () => {
  if (document.fullscreenElement) document.exitFullscreen();
  else document.documentElement.requestFullscreen();
  fsBtn.blur();
});

document.addEventListener("fullscreenchange", () => {
  const on = !!document.fullscreenElement;
  document.body.classList.toggle("fs", on);
  bar.classList.remove("show");
  fsBtn.textContent = on ? "Exit fullscreen" : "Fullscreen";
});

hot.addEventListener("mouseenter", () => bar.classList.add("show"));
bar.addEventListener("mouseleave", () => bar.classList.remove("show"));

endBtn.addEventListener("click", () => {
  if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ t: "end" }));
  ended = true;
  finish("session ended");
  ws.close();
});

img.addEventListener("mousemove", (e) => {
  const r = img.getBoundingClientRect();
  sendEvent({ t: "move", x: (e.clientX - r.left) / r.width, y: (e.clientY - r.top) / r.height });
});
img.addEventListener("mousedown", (e) => sendEvent({ t: "down", button: e.button }));
img.addEventListener("mouseup", (e) => sendEvent({ t: "up", button: e.button }));
img.addEventListener("wheel", (e) => { sendEvent({ t: "scroll", dy: e.deltaY }); e.preventDefault(); }, { passive: false });
img.addEventListener("contextmenu", (e) => e.preventDefault());
window.addEventListener("keydown", (e) => { sendEvent({ t: "keydown", key: e.key }); e.preventDefault(); });
window.addEventListener("keyup", (e) => { sendEvent({ t: "keyup", key: e.key }); e.preventDefault(); });
</script>
</body>
</html>`