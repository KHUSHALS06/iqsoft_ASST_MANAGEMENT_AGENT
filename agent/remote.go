package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	frameInterval = 150 * time.Millisecond // ~6-7 fps: plenty for support/admin use, keeps bandwidth low
	jpegQuality   = 55
)

// inputEvent mirrors the JSON the browser-side viewer page sends.
type inputEvent struct {
	T      string  `json:"t"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Button int     `json:"button"`
	Key    string  `json:"key"`
	DY     float64 `json:"dy"`
}

// runRemoteSession dials the relay for sessionID, keeps the on-screen
// indicator up for the whole call, streams frames out and applies whatever
// input comes back, until either side closes the connection.
//
// The indicator is owned here, not left to the caller: there is no path
// through this function that streams a frame before indicatorShow() has
// succeeded, and indicatorHide() always runs via defer.
func runRemoteSession(creds *Creds, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("missing session id")
	}
	wsURL := strings.Replace(serverURL, "http", "ws", 1) + "/remote/agent-ws?session=" + url.QueryEscape(sessionID)
	header := http.Header{"Authorization": {"Bearer " + creds.DeviceID + "." + creds.Secret}}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	defer conn.Close()

	if err := indicatorShow(); err != nil {
		return fmt.Errorf("cannot show session indicator, refusing to stream: %w", err)
	}
	defer indicatorHide()

	log.Println("remote: session", sessionID, "started")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var ev inputEvent
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			applyInput(ev)
		}
	}()

	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			log.Println("remote: session", sessionID, "ended")
			return nil
		case <-ticker.C:
			jpg, err := captureScreenJPEG(jpegQuality)
			if err != nil {
				log.Println("remote: capture failed:", err)
				continue
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, jpg); err != nil {
				log.Println("remote: session", sessionID, "ended")
				return nil
			}
		}
	}
}