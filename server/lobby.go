// Copyright 2026 Jonas Bartel
// SPDX-License-Identifier: Apache-2.0

package main

// Lobby & LobbyManager.
//
// Concurrency model: each Lobby owns ONE goroutine (the command loop) that
// has exclusive ownership of the Engine. WS read goroutines push commands
// onto lobby.cmds; the loop applies them serially. While Phase == "flying"
// the loop also pulls from a 30Hz ticker to advance the projectile.
//
// LobbyManager.mu only guards the map; nothing else shares state across
// goroutines without channel synchronisation.

import (
	"encoding/json"
	"log"
	"math/rand"
	"sync"
	"time"
)

// inboundCmd is what WS readers push onto Lobby.cmds.
type inboundCmd struct {
	pIdx int
	msg  ClientMsg
}

// joinReq is sent from a WS handler when a connection wants to attach.
type joinReq struct {
	conn   *Conn
	result chan joinResult
}

type joinResult struct {
	pIdx int
	err  error
}

type windEstimate struct {
	round int
	turn  int
	wind  float64
	value float64
	set   bool
}

// Lobby holds game state and the command loop.
type Lobby struct {
	code      string
	createdAt time.Time
	expiresAt time.Time

	eng *Engine

	cmds chan inboundCmd
	join chan joinReq
	quit chan *Conn // a connection is leaving
	done chan struct{}

	// Scoreboard is shared (may be nil if DB init failed at startup).
	scoreboard *Scoreboard
	// recorded is set after the first time a gameover transition has been
	// persisted, so further broadcasts don't double-count.
	recorded bool
	// ghost skips all scoreboard persistence (used by automated smoke tests).
	ghost bool

	// Single-player vs-Computer support. aiSlot is the slot the AI occupies
	// (1) or -1 if there's no AI in this lobby. aiTimer is read/written only
	// by the loop goroutine; the timer's callback only sends on cmds (channel-
	// safe) so no mutex is needed.
	aiSlot  int
	aiDiff  string
	aiTimer *time.Timer

	// Owned by the loop goroutine; never touched from outside.
	conns [2]*Conn

	// sessID is the persistent sessions-table row id per player slot (0 = none).
	// One session is recorded when a slot is first filled; reconnects to the
	// same slot reuse it instead of inserting a duplicate row.
	sessID [2]int64

	// Per-player cached blind-wind estimate. Keeps the estimate stable across
	// repeated state broadcasts within the same turn.
	windView [2]windEstimate
}

func newLobby(code, hostName, hostTeam string, sb *Scoreboard, ghost bool) *Lobby {
	l := &Lobby{
		code:       code,
		createdAt:  time.Now(),
		expiresAt:  time.Now().Add(2 * time.Hour),
		eng:        NewEng(),
		cmds:       make(chan inboundCmd, 16),
		join:       make(chan joinReq, 4),
		quit:       make(chan *Conn, 4),
		done:       make(chan struct{}),
		scoreboard: sb,
		aiSlot:     -1,
		ghost:      ghost,
	}
	l.eng.GS.Players[0] = hostName
	l.eng.GS.Teams[0] = hostTeam
	go l.loop()
	return l
}

// newAILobby creates a single-player lobby with the host in slot 0 and a
// virtual computer player pre-filling slot 1. The AI is driven by the lobby
// loop via maybeScheduleAI().
func newAILobby(code, hostName, hostTeam, diff string, sb *Scoreboard, ghost bool) *Lobby {
	l := &Lobby{
		code:       code,
		createdAt:  time.Now(),
		expiresAt:  time.Now().Add(2 * time.Hour),
		eng:        NewEng(),
		cmds:       make(chan inboundCmd, 16),
		join:       make(chan joinReq, 4),
		quit:       make(chan *Conn, 4),
		done:       make(chan struct{}),
		scoreboard: sb,
		aiSlot:     1,
		aiDiff:     diff,
		ghost:      ghost,
	}
	l.eng.GS.Players[0] = hostName
	l.eng.GS.Teams[0] = hostTeam
	l.eng.GS.Players[1] = "🤖 Computer (" + aiDiffLabel(diff) + ")"
	l.eng.GS.Teams[1] = "Sonstiges"
	go l.loop()
	return l
}

func aiDiffLabel(diff string) string {
	switch diff {
	case "easy":
		return "Leicht"
	case "hard":
		return "Schwer"
	default:
		return "Mittel"
	}
}

// loop is the Lobby's exclusive owner of the Engine.
func (l *Lobby) loop() {
	const tickHz = 30
	tick := time.NewTicker(time.Second / tickHz)
	defer tick.Stop()
	tick.Stop() // start stopped; only start while flying

	// When the lobby shuts down (GC/expiry), close out any session whose player
	// is still attached so long games that outlive the lobby get an ended_at.
	defer l.endOpenSessions()

	tickRunning := false
	startTick := func() {
		if tickRunning {
			return
		}
		tick.Reset(time.Second / tickHz)
		tickRunning = true
	}
	stopTick := func() {
		if !tickRunning {
			return
		}
		tick.Stop()
		tickRunning = false
	}

	for {
		select {
		case <-l.done:
			return

		case j := <-l.join:
			pIdx, err := l.attach(j.conn)
			j.result <- joinResult{pIdx: pIdx, err: err}
			if err == nil {
				// peer-presence notification
				l.notifyPresence(pIdx, "joined")
			}

		case c := <-l.quit:
			// Detach a leaving connection. The l.conns[i] == c guard means this
			// is the *current* socket for the slot leaving — a stale socket
			// already evicted by a reconnect won't match, so a reconnect never
			// ends the session.
			for i := 0; i < 2; i++ {
				if l.conns[i] == c {
					l.conns[i] = nil
					l.notifyPresence(i, "left")
					if !l.ghost && l.scoreboard != nil && l.sessID[i] != 0 {
						l.scoreboard.RecordSessionEnd(l.sessID[i])
					}
					break
				}
			}

		case ic := <-l.cmds:
			if ic.pIdx < 0 || ic.pIdx > 1 {
				continue
			}
			// Cancel any pending AI move — game state is about to mutate.
			if l.aiTimer != nil {
				l.aiTimer.Stop()
				l.aiTimer = nil
			}
			if err := l.eng.HandleCmd(ic.pIdx, ic.msg.Data); err != nil {
				l.sendErr(ic.pIdx, err.Error())
				// Re-arm AI in case the failed command was the human's input
				// while it was already the AI's turn (defensive — shouldn't
				// normally happen since the engine would reject the human's
				// command first).
				l.maybeScheduleAI()
				continue
			}
			if l.eng.GS.Phase == "flying" {
				startTick()
			}
			l.maybeRecordGameOver()
			l.broadcastState()
			l.maybeScheduleAI()

		case <-tick.C:
			if l.aiTimer != nil {
				l.aiTimer.Stop()
				l.aiTimer = nil
			}
			l.eng.StepProjectile()
			l.maybeRecordGameOver()
			l.broadcastState()
			if l.eng.GS.Phase != "flying" {
				stopTick()
			}
			l.maybeScheduleAI()
		}
	}
}

// maybeScheduleAI is invoked by the loop after every state mutation. If the
// lobby has an AI player and it is currently the AI's turn in the aim phase,
// it computes the AI's command synchronously (so GS access stays serialised)
// and schedules its delivery via a timer. The timer callback only sends on
// the cmds channel, which is goroutine-safe.
func (l *Lobby) maybeScheduleAI() {
	if l.aiSlot < 0 || l.aiDiff == "" {
		// No AI in this lobby — make sure no stale label lingers.
		if l.eng.GS.AIAction != "" {
			l.eng.GS.AIAction = ""
			l.broadcastState()
		}
		return
	}
	if l.aiTimer != nil {
		return
	}
	gs := &l.eng.GS
	if gs.Phase != "aim" || gs.Turn != l.aiSlot || gs.Winner != nil {
		// Not the AI's turn — clear the status overlay so it doesn't bleed into
		// the human's turn. This is the "Computer baut Mauern" text disappearing.
		if gs.AIAction != "" {
			gs.AIAction = ""
			l.broadcastState()
		}
		return
	}
	cmd := computeAICmd(gs, l.aiSlot, l.aiDiff)

	// Show what the AI is about to do during the think delay so the human can
	// tell the AI is working (especially while it places bricks one at a time).
	label := describeAICmd(cmd)
	if gs.AIAction != label {
		gs.AIAction = label
		l.broadcastState()
	}

	delay := aiCmdDelay(l.aiDiff, cmd)
	slot := l.aiSlot
	l.aiTimer = time.AfterFunc(delay, func() {
		select {
		case l.cmds <- inboundCmd{pIdx: slot, msg: ClientMsg{Type: "command", Data: cmd}}:
		case <-l.done:
		}
	})
}

// aiCmdDelay returns the think-delay before dispatching the computed AI command.
// Building/placing wall bricks is intentionally fast so repairs don't feel
// sluggish while still keeping a visible action rhythm for the player.
func aiCmdDelay(diff string, cmd Cmd) time.Duration {
	if cmd.Action == "place_brick" {
		return 120 * time.Millisecond
	}
	if cmd.Action == "buy" {
		if p, ok := cmd.Payload.(BuyP); ok && p.Item == "buy-bricks" {
			return 220 * time.Millisecond
		}
	}
	if diff == "hard" {
		return 1400 * time.Millisecond
	}
	return 800 * time.Millisecond
}

// describeAICmd returns a short German label for the AI's next move so the
// client can show it as a centered overlay during the think delay. Empty
// string means "show nothing" — used for fire (projectile is its own signal)
// and end_turn (turn handoff is its own signal).
func describeAICmd(cmd Cmd) string {
	switch cmd.Action {
	case "place_brick":
		return "Computer baut Mauern"
	case "set_tax":
		return "Computer ändert Steuern"
	case "sell_tower":
		return "Computer verkauft Förderturm"
	case "buy":
		if p, ok := cmd.Payload.(BuyP); ok {
			switch p.Item {
			case "buy-powder-50":
				return "Computer kauft Pulver"
			case "buy-ammo-5":
				return "Computer kauft Munition"
			case "buy-bricks":
				return "Computer kauft Mauersteine"
			case "buy-cannon":
				return "Computer ersetzt Kanone"
			case "buy-tower":
				return "Computer baut Förderturm"
			case "buy-windvane":
				return "Computer baut Wetterfahne"
			case "buy-shrapnel":
				return "Computer kauft Schrapnell"

			}
		}
		return "Computer kauft im Markt"
	}
	return ""
}

// attach assigns a connection to a player slot. Reconnects (same name) replace
// the existing slot; new players take any open slot. The AI slot (if any) is
// off-limits to incoming connections — a human can never steal it even by
// matching the AI's display name.
func (l *Lobby) attach(c *Conn) (int, error) {
	// Reconnect: same name already in a slot
	for i := 0; i < 2; i++ {
		if l.aiSlot >= 0 && i == l.aiSlot {
			continue
		}
		if l.eng.GS.Players[i] == c.name {
			// Close existing socket if any
			if l.conns[i] != nil && l.conns[i] != c {
				l.conns[i].closeWrite()
			}
			l.conns[i] = c
			c.pIdx = i
			// Refresh team in case it was empty (defensive — CheckTeam
			// guarantees the same team across reconnects).
			if l.eng.GS.Teams[i] == "" {
				l.eng.GS.Teams[i] = c.team
			}
			if l.sessID[i] == 0 {
				// First attach for a slot whose player name was pre-seeded at
				// lobby creation (the host, placed in slot 0). Record their
				// session now.
				l.startSession(i, c)
			} else {
				// Genuine reconnect — reuse the existing session row, reopening
				// it so a briefly-disconnected player is counted live again
				// instead of as a separate session.
				if !l.ghost && l.scoreboard != nil {
					l.scoreboard.ResumeSession(l.sessID[i])
				}
			}
			l.sendHello(i)
			return i, nil
		}
	}
	// New player: take next open slot
	for i := 0; i < 2; i++ {
		if l.aiSlot >= 0 && i == l.aiSlot {
			continue
		}
		if l.eng.GS.Players[i] == "" {
			l.eng.GS.Players[i] = c.name
			l.eng.GS.Teams[i] = c.team
			l.conns[i] = c
			c.pIdx = i
			// One session per player joining a lobby. Reconnects/reloads hit the
			// reconnect branch above and reuse the existing row.
			l.startSession(i, c)
			l.sendHello(i)
			return i, nil
		}
	}
	return -1, errLobbyFull
}

// startSession records a new sessions-table row for a player slot and stores
// its id in l.sessID. No-op for ghost lobbies or when the DB is unavailable.
func (l *Lobby) startSession(slot int, c *Conn) {
	if l.ghost || l.scoreboard == nil {
		return
	}
	if id, err := l.scoreboard.RecordSessionStart(c.name, c.team, l.gameType()); err == nil {
		l.sessID[slot] = id
	} else {
		log.Printf("scoreboard: session start failed (lobby %s): %v", l.code, err)
	}
}

// gameType returns the sessions-table game_type tag for this lobby.
func (l *Lobby) gameType() string {
	if l.aiSlot >= 0 {
		return "ai_" + l.aiDiff
	}
	return "pvp"
}

// endOpenSessions closes out any session whose player is still attached when
// the lobby shuts down.
func (l *Lobby) endOpenSessions() {
	if l.ghost || l.scoreboard == nil {
		return
	}
	for i := 0; i < 2; i++ {
		if l.conns[i] != nil && l.sessID[i] != 0 {
			l.scoreboard.RecordSessionEnd(l.sessID[i])
		}
	}
}

// maybeRecordGameOver persists the result the first time the engine enters
// the "gameover" phase with a winner set. Idempotent across the rest of the
// lobby's life via the recorded flag.
func (l *Lobby) maybeRecordGameOver() {
	if l.recorded || l.scoreboard == nil {
		return
	}
	gs := &l.eng.GS
	if gs.Phase != "gameover" || gs.Winner == nil {
		return
	}
	// Single-player matches don't count toward the public team/player scoreboard
	// but we do record AI wins/losses per difficulty separately so users can
	// view their winning rate against the computer.
	if l.aiSlot >= 0 {
		w := *gs.Winner
		playerWon := w == 0 // slot 0 is always the human in AI mode
		if !l.ghost && l.scoreboard != nil && gs.Players[0] != "" {
			if err := l.scoreboard.RecordAIMatch(gs.Players[0], l.aiDiff, playerWon); err != nil {
				log.Printf("scoreboard: record ai match failed (lobby %s): %v", l.code, err)
			}
		}
		l.recorded = true
		return
	}
	w := *gs.Winner
	if w < 0 || w > 1 {
		return
	}
	loser := 1 - w
	winnerName, loserName := gs.Players[w], gs.Players[loser]
	winnerTeam, loserTeam := gs.Teams[w], gs.Teams[loser]
	// Defensive: a half-joined lobby shouldn't reach gameover, but skip if so.
	if winnerName == "" || loserName == "" {
		l.recorded = true // don't keep retrying
		return
	}
	if !l.ghost {
		if err := l.scoreboard.RecordMatch(winnerName, loserName, winnerTeam, loserTeam, gs.Reason); err != nil {
			log.Printf("scoreboard: record match failed (lobby %s): %v", l.code, err)
		}
	}
	l.recorded = true
}

func (l *Lobby) playerSeesExactWind(pIdx int) bool {
	if pIdx < 0 || pIdx > 1 {
		return true
	}
	v := l.eng.GS.Castles[pIdx].WindVane
	return v.Alive && v.Owned
}

func (l *Lobby) estimatedWindForPlayer(pIdx int, wind float64, round, turn int) float64 {
	if pIdx < 0 || pIdx > 1 {
		return wind
	}
	c := &l.windView[pIdx]
	if !c.set || c.round != round || c.turn != turn || c.wind != wind {
		c.round = round
		c.turn = turn
		c.wind = wind
		c.value = wind * (0.5 + 0.5*rand.Float64())
		c.set = true
	}
	return c.value
}

func (l *Lobby) stateForPlayer(pIdx int) GS {
	view := l.eng.GS
	view.WindExact = l.playerSeesExactWind(pIdx)
	if view.WindExact {
		return view
	}
	view.Wind = l.estimatedWindForPlayer(pIdx, view.Wind, view.Round, view.Turn)
	return view
}

func (l *Lobby) sendHello(pIdx int) {
	stateBytes, _ := json.Marshal(l.stateForPlayer(pIdx))
	msg := ServerHello{
		Type:  "hello",
		Code:  l.code,
		You:   pIdx,
		State: json.RawMessage(stateBytes),
	}
	if b, err := json.Marshal(msg); err == nil {
		l.sendTo(pIdx, b)
	}
}

func (l *Lobby) broadcastState() {
	for i := 0; i < 2; i++ {
		stateBytes, err := json.Marshal(l.stateForPlayer(i))
		if err != nil {
			continue
		}
		msg := ServerState{Type: "state", State: json.RawMessage(stateBytes)}
		b, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		l.sendTo(i, b)
	}
}

func (l *Lobby) notifyPresence(slot int, status string) {
	other := 1 - slot
	if l.conns[other] == nil {
		return
	}
	msg := ServerOpponent{Type: "opponent", Status: status}
	if b, err := json.Marshal(msg); err == nil {
		l.sendTo(other, b)
	}
}

func (l *Lobby) sendErr(pIdx int, errMsg string) {
	msg := ServerError{Type: "error", Error: errMsg}
	if b, err := json.Marshal(msg); err == nil {
		l.sendTo(pIdx, b)
	}
}

// sendTo pushes a frame onto a connection's write channel without blocking
// the loop. If the channel is full or the conn is closed the message is dropped.
func (l *Lobby) sendTo(pIdx int, frame []byte) {
	c := l.conns[pIdx]
	if c == nil {
		return
	}
	select {
	case c.send <- frame:
	case <-c.done:
		// connection closed; drop
	default:
		// writer stalled; drop
	}
}

// ─── LobbyManager ───────────────────────────────────────────────────────────

type LobbyManager struct {
	mu      sync.RWMutex
	lobbies map[string]*Lobby

	scoreboard *Scoreboard

	gcDone chan struct{}
}

func newLobbyManager(sb *Scoreboard) *LobbyManager {
	m := &LobbyManager{
		lobbies:    make(map[string]*Lobby),
		scoreboard: sb,
		gcDone:     make(chan struct{}),
	}
	go m.gc()
	return m
}

func (m *LobbyManager) Create(hostName, hostTeam string, ghost bool) *Lobby {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Generate a code that doesn't clash
	for tries := 0; tries < 50; tries++ {
		code := genCode()
		if _, exists := m.lobbies[code]; !exists {
			l := newLobby(code, hostName, hostTeam, m.scoreboard, ghost)
			m.lobbies[code] = l
			return l
		}
	}
	return nil
}

// CreateAI creates a single-player lobby with an AI opponent at the given
// difficulty in slot 1. Mirrors Create, but uses newAILobby.
func (m *LobbyManager) CreateAI(hostName, hostTeam, diff string, ghost bool) *Lobby {
	m.mu.Lock()
	defer m.mu.Unlock()
	for tries := 0; tries < 50; tries++ {
		code := genCode()
		if _, exists := m.lobbies[code]; !exists {
			l := newAILobby(code, hostName, hostTeam, diff, m.scoreboard, ghost)
			m.lobbies[code] = l
			return l
		}
	}
	return nil
}

func (m *LobbyManager) Get(code string) (*Lobby, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	l, ok := m.lobbies[code]
	if !ok {
		return nil, false
	}
	if time.Now().After(l.expiresAt) {
		return nil, false
	}
	return l, true
}

func (m *LobbyManager) gc() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.mu.Lock()
			now := time.Now()
			for code, l := range m.lobbies {
				if now.After(l.expiresAt) {
					if l.aiTimer != nil {
						l.aiTimer.Stop()
					}
					close(l.done)
					delete(m.lobbies, code)
				}
			}
			m.mu.Unlock()
		case <-m.gcDone:
			return
		}
	}
}
