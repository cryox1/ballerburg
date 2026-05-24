// Copyright 2026 Jonas Bartel
// SPDX-License-Identifier: Apache-2.0

package main

// WebSocket handler — one endpoint at /ws.
//
//   /ws?name=N            → create a new lobby, this client is host (slot 0).
//   /ws?name=N&code=XYZ   → join (or reconnect to) lobby XYZ.
//
// On successful attach the server immediately sends a {"type":"hello",...}
// frame containing the lobby code, the player's slot index, and the full
// game state. Subsequent broadcasts are {"type":"state",...} frames.

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
)

var (
	errLobbyFull    = errors.New("lobby full")
	errBadName      = errors.New("name required")
	errLobbyMissing = errors.New("lobby not found or expired")
)

// Server-→client messages.
type ServerHello struct {
	Type  string          `json:"type"` // "hello"
	Code  string          `json:"code"`
	You   int             `json:"you"`
	State json.RawMessage `json:"state"`
}
type ServerState struct {
	Type  string          `json:"type"` // "state"
	State json.RawMessage `json:"state"`
}
type ServerError struct {
	Type  string `json:"type"` // "error"
	Error string `json:"error"`
}
type ServerOpponent struct {
	Type   string `json:"type"`   // "opponent"
	Status string `json:"status"` // "joined" | "left"
}

// Client-→server message envelope.
type ClientMsg struct {
	Type string `json:"type"` // "command"
	Data Cmd    `json:"data"`
}

// Conn is a single WebSocket connection bound to a lobby slot.
//
// Lifecycle: the writer drains `send` until `done` is closed. We never close
// `send` itself, which avoids the panic-on-send-to-closed-channel race when
// the lobby goroutine is broadcasting and a reconnect evicts the old socket.
type Conn struct {
	ws     *WSConn
	send   chan []byte
	done   chan struct{}
	name   string
	team   string
	pIdx   int
	closed bool
	mu     sync.Mutex
}

func (c *Conn) closeWrite() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.done)
}

// ─── Server ─────────────────────────────────────────────────────────────────

type Server struct {
	mgr        *LobbyManager
	scoreboard *Scoreboard
}

func newServer(sb *Scoreboard) *Server {
	return &Server{
		mgr:        newLobbyManager(sb),
		scoreboard: sb,
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	team := strings.TrimSpace(q.Get("team"))
	code := strings.TrimSpace(q.Get("code"))
	ai := strings.TrimSpace(q.Get("ai"))
	ghost := q.Get("ghost") == "1"

	if name == "" || len(name) > 32 {
		http.Error(w, "name required (max 32 chars)", http.StatusBadRequest)
		return
	}
	if s.scoreboard == nil {
		http.Error(w, "scoreboard unavailable", http.StatusServiceUnavailable)
		return
	}
	if ok, err := s.scoreboard.IsAllowedTeam(team); err != nil {
		http.Error(w, "team lookup failed", http.StatusInternalServerError)
		return
	} else if !ok {
		http.Error(w, "team required: unknown team", http.StatusBadRequest)
		return
	}
	if ai != "" && ai != "easy" && ai != "medium" && ai != "hard" {
		http.Error(w, "ai must be one of easy, medium, hard", http.StatusBadRequest)
		return
	}
	if ai != "" && code != "" {
		http.Error(w, "ai mode does not accept a lobby code", http.StatusBadRequest)
		return
	}

	// Team-lock: if this name already exists in the persisted scoreboard with a
	// different team, refuse before doing anything else. This check does not
	// create a database row; abandoned lobbies should not persist names.
	// Ghost connections skip this check since they never write to the DB.
	if !ghost && s.scoreboard != nil {
		if err := s.scoreboard.CheckTeam(name, team); err != nil {
			if errors.Is(err, ErrTeamMismatch) {
				http.Error(w, err.Error(), http.StatusConflict)
			} else {
				log.Printf("scoreboard: team check failed: %v", err)
				http.Error(w, "scoreboard unavailable", http.StatusInternalServerError)
			}
			return
		}
	}

	// Resolve lobby BEFORE upgrading so we can return a clean HTTP error.
	var lobby *Lobby
	if code == "" {
		if ai != "" {
			lobby = s.mgr.CreateAI(name, team, ai, ghost)
		} else {
			lobby = s.mgr.Create(name, team, ghost)
		}
		if lobby == nil {
			http.Error(w, "could not create lobby", http.StatusInternalServerError)
			return
		}
	} else {
		var ok bool
		lobby, ok = s.mgr.Get(code)
		if !ok {
			http.Error(w, "lobby not found or expired", http.StatusNotFound)
			return
		}
	}

	wsConn, err := UpgradeHTTP(w, r)
	if err != nil {
		log.Printf("ws upgrade failed: %v", err)
		return
	}

	c := &Conn{
		ws:   wsConn,
		send: make(chan []byte, 32),
		done: make(chan struct{}),
		name: name,
		team: team,
	}

	// Ask the lobby to attach this connection.
	resCh := make(chan joinResult, 1)
	select {
	case lobby.join <- joinReq{conn: c, result: resCh}:
	case <-lobby.done:
		writeError(wsConn, "lobby closed")
		wsConn.Close()
		return
	}
	res := <-resCh
	if res.err != nil {
		writeError(wsConn, res.err.Error())
		wsConn.Close()
		return
	}

	// Session start/end is recorded by the lobby (see Lobby.attach), so a
	// reconnect or page reload reuses the same session instead of inserting a
	// duplicate sessions row.

	// Spawn writer goroutine.
	go c.writeLoop()

	// Read loop runs inline — blocks until the connection closes.
	c.readLoop(lobby)
}

func writeError(wsConn *WSConn, msg string) {
	b, _ := json.Marshal(ServerError{Type: "error", Error: msg})
	wsConn.WriteRaw(b, WS_OP_TEXT)
}

func (c *Conn) writeLoop() {
	defer c.ws.Close()
	for {
		select {
		case frame := <-c.send:
			if err := c.ws.WriteRaw(frame, WS_OP_TEXT); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *Conn) readLoop(lobby *Lobby) {
	defer func() {
		// Tell the lobby we left.
		select {
		case lobby.quit <- c:
		case <-lobby.done:
		}
		c.closeWrite()
	}()

	for {
		op, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		switch op {
		case WS_OP_CLOSE:
			return
		case WS_OP_PING:
			// echo back as PONG (RFC 6455 §5.5.3)
			c.ws.WriteRaw(data, WS_OP_PONG)
			continue
		case WS_OP_PONG:
			continue
		case WS_OP_TEXT:
			// Decode the envelope first, then specialise the payload struct.
			var raw struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(data, &raw); err != nil {
				continue
			}
			if raw.Type != "command" {
				continue
			}
			var cmdEnv struct {
				Action  string          `json:"action"`
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(raw.Data, &cmdEnv); err != nil {
				continue
			}
			cmd := Cmd{Action: cmdEnv.Action}
			switch cmdEnv.Action {
			case "fire":
				var p FireP
				if len(cmdEnv.Payload) > 0 {
					_ = json.Unmarshal(cmdEnv.Payload, &p)
				}
				cmd.Payload = p
			case "buy":
				var p BuyP
				if len(cmdEnv.Payload) > 0 {
					_ = json.Unmarshal(cmdEnv.Payload, &p)
				}
				cmd.Payload = p
			case "set_tax":
				var p TaxP
				if len(cmdEnv.Payload) > 0 {
					_ = json.Unmarshal(cmdEnv.Payload, &p)
				}
				cmd.Payload = p
			case "place_brick":
				var p PlaceBrickP
				if len(cmdEnv.Payload) > 0 {
					_ = json.Unmarshal(cmdEnv.Payload, &p)
				}
				cmd.Payload = p
			case "sell_tower":
				var p SellTowerP
				if len(cmdEnv.Payload) > 0 {
					_ = json.Unmarshal(cmdEnv.Payload, &p)
				}
				cmd.Payload = p
			case "end_turn", "surrender":
				// no payload
			default:
				continue
			}

			select {
			case lobby.cmds <- inboundCmd{pIdx: c.pIdx, msg: ClientMsg{Type: "command", Data: cmd}}:
			case <-lobby.done:
				return
			}
		}
	}
}
