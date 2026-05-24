// Copyright 2026 Jonas Bartel
// SPDX-License-Identifier: Apache-2.0

package main

// Persistent scoreboard backed by SQLite (pure-Go modernc.org/sqlite driver,
// works with CGO_ENABLED=0).
//
// Two tables:
//   players(name PK COLLATE NOCASE, team, wins, losses, last_played)
//   matches(id, winner, loser, winner_team, loser_team, reason, ended_at)
//
// Team is locked to a name once a completed result is persisted. Re-using a
// persisted name with a different team returns ErrTeamMismatch — the WS handler
// maps this to a 409 before upgrading.

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"log"

	_ "modernc.org/sqlite"
)

// defaultTeams are seeded into the `teams` table on first boot. After that the
// list is fully editable from the admin panel.
var defaultTeams = []struct {
	Name      string
	SortOrder int
}{
	{"Wirtschaftsinformatik", 0},
	{"Informatik", 1},
	{"Sonstiges", 2},
}

// ErrTeamMismatch is returned by CheckTeam/ensurePlayerTx when a name already exists
// with a different team. The error message includes the registered team so
// the client can show a useful hint.
var ErrTeamMismatch = errors.New("team mismatch")

// ErrTeamInUse is returned by DeleteTeam when players/matches/sessions still
// reference the team being deleted. Admin must reassign first.
var ErrTeamInUse = errors.New("team in use")

// ErrTeamExists is returned when trying to add/rename to a name that already exists.
var ErrTeamExists = errors.New("team already exists")

// ErrTeamUnknown is returned when a rename/delete targets a team that doesn't exist.
var ErrTeamUnknown = errors.New("team not found")

type Scoreboard struct {
	db *sql.DB
	// Serialise writes ourselves: SQLite handles concurrent reads but a busy
	// writer can hit "database is locked" under load. The lobby + WS layer is
	// already low-volume (one record per finished game) so a single mutex
	// keeps the code simple.
	wmu sync.Mutex
}

func OpenScoreboard(path string) (*Scoreboard, error) {
	// _journal=WAL gives concurrent readers; _busy_timeout retries automatically.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}
	schema := `
CREATE TABLE IF NOT EXISTS players (
  name        TEXT PRIMARY KEY COLLATE NOCASE,
  team        TEXT NOT NULL,
  wins        INTEGER NOT NULL DEFAULT 0,
  losses      INTEGER NOT NULL DEFAULT 0,
  last_played DATETIME
);
CREATE TABLE IF NOT EXISTS matches (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  winner      TEXT NOT NULL,
  loser       TEXT NOT NULL,
  winner_team TEXT NOT NULL,
  loser_team  TEXT NOT NULL,
  reason      TEXT,
  ended_at    DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS ai_matches (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  player      TEXT NOT NULL,
  difficulty  TEXT NOT NULL,
  won         INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  player        TEXT NOT NULL,
  team          TEXT NOT NULL,
  game_type     TEXT NOT NULL,
  started_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
  ended_at      DATETIME,
  duration_secs INTEGER
);
CREATE TABLE IF NOT EXISTS teams (
  name       TEXT PRIMARY KEY COLLATE NOCASE,
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	for _, t := range defaultTeams {
		if _, err := db.Exec(
			`INSERT OR IGNORE INTO teams(name, sort_order) VALUES(?, ?)`,
			t.Name, t.SortOrder,
		); err != nil {
			db.Close()
			return nil, fmt.Errorf("seed teams: %w", err)
		}
	}
	return &Scoreboard{db: db}, nil
}

func (s *Scoreboard) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// TeamMeta is the JSON shape of a team row.
type TeamMeta struct {
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
}

func (s *Scoreboard) ListTeams() ([]TeamMeta, error) {
	rows, err := s.db.Query(`SELECT name, sort_order FROM teams ORDER BY sort_order, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamMeta{}
	for rows.Next() {
		var t TeamMeta
		if err := rows.Scan(&t.Name, &t.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// IsAllowedTeam reports whether the given team name exists in the teams table.
// Lookup is case-insensitive (the column uses COLLATE NOCASE).
func (s *Scoreboard) IsAllowedTeam(name string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM teams WHERE name = ?`, name).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Scoreboard) AddTeam(name string, sortOrder int) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO teams(name, sort_order) VALUES(?, ?)`,
		name, sortOrder,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrTeamExists
	}
	return nil
}

func (s *Scoreboard) UpdateTeamSort(name string, sortOrder int) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.Exec(`UPDATE teams SET sort_order = ? WHERE name = ?`, sortOrder, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrTeamUnknown
	}
	return nil
}

// RenameTeam renames a team and cascades the change through every row that
// references it (players.team, matches.winner_team/loser_team, sessions.team)
// in a single transaction. The old and new names are compared case-insensitively
// for the existence check, so renaming "Sonstiges" -> "sonstiges" is allowed.
func (s *Scoreboard) RenameTeam(oldName, newName string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	if oldName == newName {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Verify the source exists.
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM teams WHERE name = ?`, oldName).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrTeamUnknown
	}
	// Reject if the new name collides with a different existing team.
	if !strings.EqualFold(oldName, newName) {
		var clash int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM teams WHERE name = ?`, newName).Scan(&clash); err != nil {
			return err
		}
		if clash > 0 {
			return ErrTeamExists
		}
	}

	if _, err := tx.Exec(`UPDATE teams SET name = ? WHERE name = ?`, newName, oldName); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE players SET team = ? WHERE team = ?`, newName, oldName); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE matches SET winner_team = ? WHERE winner_team = ?`, newName, oldName); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE matches SET loser_team = ? WHERE loser_team = ?`, newName, oldName); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE sessions SET team = ? WHERE team = ?`, newName, oldName); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteTeam removes a team. Fails with ErrTeamInUse if any player/match/session
// still references it.
func (s *Scoreboard) DeleteTeam(name string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	for _, q := range []string{
		`SELECT COUNT(*) FROM players WHERE team = ?`,
		`SELECT COUNT(*) FROM matches WHERE winner_team = ? OR loser_team = ?`,
		`SELECT COUNT(*) FROM sessions WHERE team = ?`,
	} {
		var n int
		var err error
		if strings.Contains(q, "OR") {
			err = s.db.QueryRow(q, name, name).Scan(&n)
		} else {
			err = s.db.QueryRow(q, name).Scan(&n)
		}
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrTeamInUse
		}
	}
	res, err := s.db.Exec(`DELETE FROM teams WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrTeamUnknown
	}
	return nil
}

// CheckTeam is the team-lock guard for persisted scoreboard names. It does not
// create a player record; records are created only when a result is stored.
func (s *Scoreboard) CheckTeam(name, team string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	var existing string
	row := s.db.QueryRow(`SELECT team FROM players WHERE name = ?`, name)
	switch err := row.Scan(&existing); err {
	case nil:
		if !strings.EqualFold(existing, team) {
			return fmt.Errorf("%w: %q is registered with team %q", ErrTeamMismatch, name, existing)
		}
		return nil
	case sql.ErrNoRows:
		return nil
	default:
		return err
	}
}

// RecordMatch stores one finished match and bumps the per-player counters
// in a single transaction. Player records are created on first completed
// result, which avoids persisting names for abandoned lobbies.
func (s *Scoreboard) RecordMatch(winner, loser, winnerTeam, loserTeam, reason string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	if err := ensurePlayerTx(tx, winner, winnerTeam); err != nil {
		return err
	}
	if err := ensurePlayerTx(tx, loser, loserTeam); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO matches(winner, loser, winner_team, loser_team, reason, ended_at) VALUES(?,?,?,?,?,?)`,
		winner, loser, winnerTeam, loserTeam, reason, now,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE players SET wins = wins + 1, last_played = ? WHERE name = ?`,
		now, winner,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE players SET losses = losses + 1, last_played = ? WHERE name = ?`,
		now, loser,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func ensurePlayerTx(tx *sql.Tx, name, team string) error {
	var existing string
	row := tx.QueryRow(`SELECT team FROM players WHERE name = ?`, name)
	switch err := row.Scan(&existing); err {
	case nil:
		if !strings.EqualFold(existing, team) {
			return fmt.Errorf("%w: %q is registered with team %q", ErrTeamMismatch, name, existing)
		}
		return nil
	case sql.ErrNoRows:
		_, err := tx.Exec(
			`INSERT INTO players(name, team, wins, losses) VALUES(?, ?, 0, 0)`,
			name, team,
		)
		return err
	default:
		return err
	}
}

// ScoreboardSnapshot is the JSON shape returned by GET /scoreboard.
type ScoreboardSnapshot struct {
	Teams        []TeamRow                `json:"teams"`
	Players      []PlayerRow              `json:"players"`
	Difficulties map[string][]AIPlayerRow `json:"difficulties"`
}

type TeamRow struct {
	Team   string `json:"team"`
	Wins   int    `json:"wins"`
	Losses int    `json:"losses"`
}

type PlayerRow struct {
	Name       string `json:"name"`
	Team       string `json:"team"`
	Wins       int    `json:"wins"`
	Losses     int    `json:"losses"`
	LastPlayed string `json:"last_played,omitempty"`
}

type AIPlayerRow struct {
	Name   string `json:"name"`
	Wins   int    `json:"wins"`
	Losses int    `json:"losses"`
}

func (s *Scoreboard) Snapshot() (ScoreboardSnapshot, error) {
	out := ScoreboardSnapshot{
		Teams:        []TeamRow{},
		Players:      []PlayerRow{},
		Difficulties: make(map[string][]AIPlayerRow),
	}

	rows, err := s.db.Query(`
		SELECT name, team, wins, losses,
		       COALESCE(strftime('%Y-%m-%dT%H:%M:%SZ', last_played), '')
		FROM players
	`)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	teamTotals := map[string]*TeamRow{}
	if teams, err := s.ListTeams(); err == nil {
		for _, t := range teams {
			teamTotals[t.Name] = &TeamRow{Team: t.Name}
		}
	}

	for rows.Next() {
		var p PlayerRow
		if err := rows.Scan(&p.Name, &p.Team, &p.Wins, &p.Losses, &p.LastPlayed); err != nil {
			return out, err
		}
		out.Players = append(out.Players, p)
		if tt, ok := teamTotals[p.Team]; ok {
			tt.Wins += p.Wins
			tt.Losses += p.Losses
		} else {
			// Unknown team — surface it anyway so admins notice drift.
			teamTotals[p.Team] = &TeamRow{Team: p.Team, Wins: p.Wins, Losses: p.Losses}
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	// Player ranking: wins desc, losses asc, name asc for stable order.
	sort.SliceStable(out.Players, func(i, j int) bool {
		a, b := out.Players[i], out.Players[j]
		if a.Wins != b.Wins {
			return a.Wins > b.Wins
		}
		if a.Losses != b.Losses {
			return a.Losses < b.Losses
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})

	for _, t := range teamTotals {
		out.Teams = append(out.Teams, *t)
	}
	sort.SliceStable(out.Teams, func(i, j int) bool {
		a, b := out.Teams[i], out.Teams[j]
		if a.Wins != b.Wins {
			return a.Wins > b.Wins
		}
		return a.Team < b.Team
	})

	ai, _ := s.SnapshotAI()
	out.Difficulties = ai

	return out, nil
}

func (s *Scoreboard) RecordAIMatch(player, difficulty string, won bool) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	w := 0
	if won {
		w = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO ai_matches(player, difficulty, won) VALUES(?,?,?)`,
		player, difficulty, w,
	)
	return err
}

func (s *Scoreboard) RecordSessionStart(player, team, gameType string) (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.Exec(
		`INSERT INTO sessions(player, team, game_type) VALUES(?,?,?)`,
		player, team, gameType,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Scoreboard) RecordSessionEnd(id int64) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _ = s.db.Exec(
		`UPDATE sessions SET ended_at = datetime('now'),
		 duration_secs = CAST((julianday('now') - julianday(started_at)) * 86400 AS INTEGER)
		 WHERE id = ?`,
		id,
	)
}

// ResumeSession reopens a session whose connection dropped and reconnected,
// so a briefly-disconnected player is counted live again rather than being
// recorded as a separate session.
func (s *Scoreboard) ResumeSession(id int64) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _ = s.db.Exec(
		`UPDATE sessions SET ended_at = NULL, duration_secs = NULL WHERE id = ?`,
		id,
	)
}

func (s *Scoreboard) SnapshotAdmin(live LiveStats) (AdminSnapshot, error) {
	snap := AdminSnapshot{
		Live:                 live,
		GameTypeBreakdown:    map[string]int{},
		DailyMatches:         []DayCount{},
		TopPlayersBySessions: []SessionPlayer{},
		Teams:                []TeamMeta{},
	}
	if teams, err := s.ListTeams(); err == nil {
		snap.Teams = teams
	}

	// Active players: sessions still open within the last 2h (handles restarts).
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE ended_at IS NULL AND started_at >= datetime('now','-2 hours')`).Scan(&snap.Live.ActivePlayers)

	s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE date(started_at) = date('now')`).Scan(&snap.SessionsToday)
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE started_at >= datetime('now','-7 days')`).Scan(&snap.SessionsWeek)
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&snap.SessionsTotal)

	s.db.QueryRow(`SELECT COUNT(*) FROM matches WHERE date(ended_at) = date('now')`).Scan(&snap.MatchesToday)
	s.db.QueryRow(`SELECT COUNT(*) FROM matches WHERE ended_at >= datetime('now','-7 days')`).Scan(&snap.MatchesWeek)
	s.db.QueryRow(`SELECT COUNT(*) FROM matches`).Scan(&snap.MatchesTotal)

	// Hourly sessions last 24h (by UTC hour-of-day).
	rows, err := s.db.Query(`SELECT CAST(strftime('%H', started_at) AS INTEGER), COUNT(*) FROM sessions WHERE started_at >= datetime('now','-24 hours') GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var h, cnt int
			if rows.Scan(&h, &cnt) == nil && h >= 0 && h < 24 {
				snap.HourlySessions[h] = cnt
			}
		}
		rows.Close()
	}

	// Daily match counts last 7 days.
	now := time.Now().UTC()
	dayMap := map[string]int{}
	for i := 6; i >= 0; i-- {
		dayMap[now.AddDate(0, 0, -i).Format("2006-01-02")] = 0
	}
	rows, err = s.db.Query(`SELECT date(ended_at), COUNT(*) FROM matches WHERE ended_at >= datetime('now','-7 days') GROUP BY 1 ORDER BY 1`)
	if err == nil {
		for rows.Next() {
			var day string
			var cnt int
			if rows.Scan(&day, &cnt) == nil {
				dayMap[day] = cnt
			}
		}
		rows.Close()
	}
	for i := 6; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		snap.DailyMatches = append(snap.DailyMatches, DayCount{Day: d, Count: dayMap[d]})
	}

	// Game type breakdown.
	rows, err = s.db.Query(`SELECT game_type, COUNT(*) FROM sessions GROUP BY game_type`)
	if err == nil {
		for rows.Next() {
			var gt string
			var cnt int
			if rows.Scan(&gt, &cnt) == nil {
				snap.GameTypeBreakdown[gt] = cnt
			}
		}
		rows.Close()
	}

	// Top 10 players by session count.
	rows, err = s.db.Query(`SELECT player, COUNT(*) AS cnt FROM sessions GROUP BY player ORDER BY cnt DESC LIMIT 10`)
	if err == nil {
		for rows.Next() {
			var p SessionPlayer
			if rows.Scan(&p.Name, &p.Count) == nil {
				snap.TopPlayersBySessions = append(snap.TopPlayersBySessions, p)
			}
		}
		rows.Close()
	}

	return snap, nil
}

func (s *Scoreboard) SnapshotAI() (map[string][]AIPlayerRow, error) {
	difficulties := []string{"easy", "medium", "hard"}
	out := make(map[string][]AIPlayerRow)

	for _, diff := range difficulties {
		rows, err := s.db.Query(`
			SELECT player,
			       SUM(won) as wins,
			       SUM(1 - won) as losses
			FROM ai_matches
			WHERE difficulty = ?
			GROUP BY player
			ORDER BY SUM(won) DESC, SUM(1 - won) ASC
		`, diff)
		if err != nil {
			log.Printf("scoreboard: SnapshotAI query failed for difficulty %q: %v", diff, err)
			continue
		}

		var list []AIPlayerRow
		for rows.Next() {
			var p AIPlayerRow
			if err := rows.Scan(&p.Name, &p.Wins, &p.Losses); err != nil {
				continue
			}
			list = append(list, p)
		}
		rows.Close()
		out[diff] = list
	}

	return out, nil
}
