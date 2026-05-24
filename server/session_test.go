// Copyright 2026 Jonas Bartel
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"
	"time"
)

// newTestConn builds a Conn with just the fields attach/quit and the lobby's
// send path touch. No real websocket is needed — sendTo never blocks because
// send is buffered and sendTo has a default case.
func newTestConn(name, team string) *Conn {
	return &Conn{
		send: make(chan []byte, 32),
		done: make(chan struct{}),
		name: name,
		team: team,
	}
}

// attachTestConn drives a join through the lobby loop and returns the slot.
func attachTestConn(t *testing.T, l *Lobby, c *Conn) int {
	t.Helper()
	res := make(chan joinResult, 1)
	select {
	case l.join <- joinReq{conn: c, result: res}:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out sending join")
	}
	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("attach failed: %v", r.err)
		}
		return r.pIdx
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for join result")
	}
	return -1
}

func sessionCount(t *testing.T, sb *Scoreboard, where string) int {
	t.Helper()
	q := "SELECT COUNT(*) FROM sessions"
	if where != "" {
		q += " WHERE " + where
	}
	var n int
	if err := sb.db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", q, err)
	}
	return n
}

// TestLobbySessionDedupOnReconnect verifies that reconnects to the same lobby
// slot reuse one sessions row instead of inserting a duplicate, and that a
// genuine departure ends the session.
func TestLobbySessionDedupOnReconnect(t *testing.T) {
	sb, err := OpenScoreboard(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open scoreboard: %v", err)
	}
	defer sb.Close()

	l := newLobby("TST1", "alice", "Informatik", sb, false)
	defer close(l.done)

	// Host's first attach records one session.
	if got := attachTestConn(t, l, newTestConn("alice", "Informatik")); got != 0 {
		t.Fatalf("alice should take slot 0, got %d", got)
	}
	if n := sessionCount(t, sb, ""); n != 1 {
		t.Fatalf("after first join: want 1 session, got %d", n)
	}

	// Two reconnects of the same player must NOT add rows.
	attachTestConn(t, l, newTestConn("alice", "Informatik"))
	attachTestConn(t, l, newTestConn("alice", "Informatik"))
	if n := sessionCount(t, sb, ""); n != 1 {
		t.Fatalf("after 2 reconnects: want 1 session, got %d", n)
	}

	// A genuinely different player adds exactly one row.
	bob := newTestConn("bob", "Wirtschaftsinformatik")
	if got := attachTestConn(t, l, bob); got != 1 {
		t.Fatalf("bob should take slot 1, got %d", got)
	}
	if n := sessionCount(t, sb, ""); n != 2 {
		t.Fatalf("after bob joins: want 2 sessions, got %d", n)
	}
	if n := sessionCount(t, sb, "game_type = 'pvp'"); n != 2 {
		t.Fatalf("want 2 pvp sessions, got %d", n)
	}

	// The current connection for each slot leaving ends its session.
	aliceCur := newTestConn("alice", "Informatik")
	attachTestConn(t, l, aliceCur)
	l.quit <- aliceCur
	l.quit <- bob
	deadline := time.Now().Add(2 * time.Second)
	for sessionCount(t, sb, "ended_at IS NOT NULL") != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("sessions did not get ended_at, %d ended",
				sessionCount(t, sb, "ended_at IS NOT NULL"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
