// Copyright 2026 Jonas Bartel
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
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
	Live                 LiveStats       `json:"live"`
	SessionsToday        int             `json:"sessions_today"`
	SessionsWeek         int             `json:"sessions_week"`
	SessionsTotal        int             `json:"sessions_total"`
	MatchesToday         int             `json:"matches_today"`
	MatchesWeek          int             `json:"matches_week"`
	MatchesTotal         int             `json:"matches_total"`
	HourlySessions       [24]int         `json:"hourly_sessions"`
	DailyMatches         []DayCount      `json:"daily_matches"`
	GameTypeBreakdown    map[string]int  `json:"game_type_breakdown"`
	TopPlayersBySessions []SessionPlayer `json:"top_players_by_sessions"`
	Teams                []TeamMeta      `json:"teams"`
	Players              []PlayerRow     `json:"players"`
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

// handleTeams is a public read-only endpoint used by index.html to populate
// the team dropdown. No auth required — the list is already visible on the
// scoreboard and we want the dropdown to work for anonymous visitors.
func (s *Server) handleTeams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scoreboard == nil {
		http.Error(w, "scoreboard unavailable", http.StatusServiceUnavailable)
		return
	}
	teams, err := s.scoreboard.ListTeams()
	if err != nil {
		log.Printf("teams: list failed: %v", err)
		http.Error(w, "teams error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=30")
	if err := json.NewEncoder(w).Encode(teams); err != nil {
		log.Printf("teams: encode failed: %v", err)
	}
}

type teamRequest struct {
	Name      string `json:"name"`
	OldName   string `json:"old_name"`
	NewName   string `json:"new_name"`
	SortOrder *int   `json:"sort_order,omitempty"`
}

// validateTeamName trims and length-checks an incoming team name. Returns the
// cleaned value or an error suitable for an HTTP 400 response.
func validateTeamName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name required")
	}
	if len(name) > 32 {
		return "", errors.New("name too long (max 32 chars)")
	}
	return name, nil
}

// handleAdminTeams handles POST/PATCH/DELETE on /api/admin/teams. Same token
// gate as /api/admin.
func (s *Server) handleAdminTeams(w http.ResponseWriter, r *http.Request) {
	token := os.Getenv("BALLERBURG_ADMIN_TOKEN")
	if token == "" || r.URL.Query().Get("token") != token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.scoreboard == nil {
		http.Error(w, "scoreboard unavailable", http.StatusServiceUnavailable)
		return
	}

	var req teamRequest
	if r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	switch r.Method {
	case http.MethodPost:
		name, err := validateTeamName(req.Name)
		if err != nil {
			writeAdminTeamsError(w, http.StatusBadRequest, err.Error())
			return
		}
		sort := 0
		if req.SortOrder != nil {
			sort = *req.SortOrder
		}
		if err := s.scoreboard.AddTeam(name, sort); err != nil {
			writeAdminTeamsTypedError(w, err)
			return
		}
	case http.MethodPatch:
		old, err := validateTeamName(req.OldName)
		if err != nil {
			writeAdminTeamsError(w, http.StatusBadRequest, "old_name: "+err.Error())
			return
		}
		if strings.TrimSpace(req.NewName) != "" {
			newName, err := validateTeamName(req.NewName)
			if err != nil {
				writeAdminTeamsError(w, http.StatusBadRequest, "new_name: "+err.Error())
				return
			}
			if err := s.scoreboard.RenameTeam(old, newName); err != nil {
				writeAdminTeamsTypedError(w, err)
				return
			}
			old = newName
		}
		if req.SortOrder != nil {
			if err := s.scoreboard.UpdateTeamSort(old, *req.SortOrder); err != nil {
				writeAdminTeamsTypedError(w, err)
				return
			}
		}
	case http.MethodDelete:
		name, err := validateTeamName(req.Name)
		if err != nil {
			writeAdminTeamsError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.scoreboard.DeleteTeam(name); err != nil {
			writeAdminTeamsTypedError(w, err)
			return
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	teams, _ := s.scoreboard.ListTeams()
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "teams": teams})
}

func writeAdminTeamsError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeAdminTeamsTypedError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTeamExists):
		writeAdminTeamsError(w, http.StatusConflict, "team already exists")
	case errors.Is(err, ErrTeamInUse):
		writeAdminTeamsError(w, http.StatusConflict, "team in use — reassign players first")
	case errors.Is(err, ErrTeamUnknown):
		writeAdminTeamsError(w, http.StatusNotFound, "team not found")
	default:
		log.Printf("admin teams: %v", err)
		writeAdminTeamsError(w, http.StatusInternalServerError, "internal error")
	}
}

type playerRequest struct {
	Name    string `json:"name"`
	OldName string `json:"old_name"`
	NewName string `json:"new_name"`
	Team    string `json:"team"`
}

// handleAdminPlayers handles PATCH/DELETE on /api/admin/players. Same token
// gate as /api/admin. No POST — player rows are created by completed matches,
// not by the admin.
func (s *Server) handleAdminPlayers(w http.ResponseWriter, r *http.Request) {
	token := os.Getenv("BALLERBURG_ADMIN_TOKEN")
	if token == "" || r.URL.Query().Get("token") != token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.scoreboard == nil {
		http.Error(w, "scoreboard unavailable", http.StatusServiceUnavailable)
		return
	}

	var req playerRequest
	if r.Method == http.MethodPatch || r.Method == http.MethodDelete {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	switch r.Method {
	case http.MethodPatch:
		old, err := validatePlayerName(req.OldName)
		if err != nil {
			writeAdminPlayersError(w, http.StatusBadRequest, "old_name: "+err.Error())
			return
		}
		current := old
		if strings.TrimSpace(req.NewName) != "" {
			newName, err := validatePlayerName(req.NewName)
			if err != nil {
				writeAdminPlayersError(w, http.StatusBadRequest, "new_name: "+err.Error())
				return
			}
			if err := s.scoreboard.RenamePlayer(old, newName); err != nil {
				writeAdminPlayersTypedError(w, err)
				return
			}
			current = newName
		}
		if strings.TrimSpace(req.Team) != "" {
			team := strings.TrimSpace(req.Team)
			if err := s.scoreboard.SetPlayerTeam(current, team); err != nil {
				writeAdminPlayersTypedError(w, err)
				return
			}
		}
	case http.MethodDelete:
		name, err := validatePlayerName(req.Name)
		if err != nil {
			writeAdminPlayersError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.scoreboard.DeletePlayer(name); err != nil {
			writeAdminPlayersTypedError(w, err)
			return
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	players, _ := s.scoreboard.ListPlayersAdmin()
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "players": players})
}

// validatePlayerName matches the cap enforced by wshandler.go on incoming names.
func validatePlayerName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name required")
	}
	if len(name) > 32 {
		return "", errors.New("name too long (max 32 chars)")
	}
	return name, nil
}

func writeAdminPlayersError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeAdminPlayersTypedError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrPlayerExists):
		writeAdminPlayersError(w, http.StatusConflict, "player name already exists")
	case errors.Is(err, ErrPlayerUnknown):
		writeAdminPlayersError(w, http.StatusNotFound, "player not found")
	case errors.Is(err, ErrTeamUnknown):
		writeAdminPlayersError(w, http.StatusBadRequest, "team does not exist")
	default:
		log.Printf("admin players: %v", err)
		writeAdminPlayersError(w, http.StatusInternalServerError, "internal error")
	}
}
