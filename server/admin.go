// Copyright 2026 Jonas Bartel
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// LiveStats holds in-memory counts from the LobbyManager.
type LiveStats struct {
	ActiveLobbies int `json:"active_lobbies"`
	ActivePlayers int `json:"active_players"` // filled from DB in SnapshotAdmin
	PvPLobbies    int `json:"pvp_lobbies"`
	AILobbies     int `json:"ai_lobbies"`
}

// AdminSnapshot is the JSON shape returned by GET /api/admin.
type AdminSnapshot struct {
	Live                 LiveStats      `json:"live"`
	SessionsToday        int            `json:"sessions_today"`
	SessionsWeek         int            `json:"sessions_week"`
	SessionsTotal        int            `json:"sessions_total"`
	MatchesToday         int            `json:"matches_today"`
	MatchesWeek          int            `json:"matches_week"`
	MatchesTotal         int            `json:"matches_total"`
	HourlySessions       [24]int        `json:"hourly_sessions"`
	DailyMatches         []DayCount     `json:"daily_matches"`
	GameTypeBreakdown    map[string]int `json:"game_type_breakdown"`
	TopPlayersBySessions []SessionPlayer `json:"top_players_by_sessions"`
}

type DayCount struct {
	Day   string `json:"day"`
	Count int    `json:"count"`
}

type SessionPlayer struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// LiveStats counts active lobbies and classifies them by type.
// Only reads immutable lobby fields (aiSlot, expiresAt) so no extra lock is needed.
func (m *LobbyManager) LiveStats() LiveStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := time.Now()
	var stats LiveStats
	for _, l := range m.lobbies {
		if now.After(l.expiresAt) {
			continue
		}
		stats.ActiveLobbies++
		if l.aiSlot == -1 {
			stats.PvPLobbies++
		} else {
			stats.AILobbies++
		}
	}
	return stats
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := os.Getenv("BALLERBURG_ADMIN_TOKEN")
	if token == "" || r.URL.Query().Get("token") != token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if s.scoreboard == nil {
		http.Error(w, "scoreboard unavailable", http.StatusServiceUnavailable)
		return
	}

	live := s.mgr.LiveStats()
	snap, err := s.scoreboard.SnapshotAdmin(live)
	if err != nil {
		log.Printf("admin: snapshot failed: %v", err)
		http.Error(w, "admin error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		log.Printf("admin: encode failed: %v", err)
	}
}
