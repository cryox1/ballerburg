// Copyright 2026 Jonas Bartel
// SPDX-License-Identifier: Apache-2.0

package main

// Ballerburg multiplayer server — entry point.
//
// Endpoints:
//   /ws?name=N&team=T            create a new lobby (caller is host).
//   /ws?name=N&team=T&code=XYZ   join (or reconnect to) lobby XYZ.
//   /scoreboard                  GET — JSON scoreboard snapshot.
//
// Lobby gameplay code is in engine.go; transport in ws.go and wshandler.go;
// persistent scoreboard in scoreboard.go.

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	dbPath := os.Getenv("BALLERBURG_DB")
	if dbPath == "" {
		dbPath = "/app/data/scoreboard.db"
	}
	sb, err := OpenScoreboard(dbPath)
	if err != nil {
		log.Fatalf("scoreboard: open %q: %v", dbPath, err)
	}
	defer sb.Close()
	log.Printf("scoreboard: opened %s", dbPath)

	allowedOrigins := parseAllowedOrigins(os.Getenv("BALLERBURG_ALLOWED_ORIGINS"))
	if len(allowedOrigins) == 0 {
		log.Println("warn: BALLERBURG_ALLOWED_ORIGINS empty — accepting WebSocket upgrades from any Origin (set this in production)")
	} else {
		log.Printf("WebSocket Origin allowlist: %v", allowedOrigins)
	}
	adminToken := os.Getenv("BALLERBURG_ADMIN_TOKEN")
	if adminToken == "" {
		log.Println("warn: BALLERBURG_ADMIN_TOKEN empty — admin endpoints will reject every request")
	}

	srv := newServer(sb, allowedOrigins, adminToken)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.handleWS)
	mux.HandleFunc("/scoreboard", srv.handleScoreboard)
	mux.HandleFunc("/api/admin", srv.handleAdmin)
	mux.HandleFunc("/api/admin/teams", srv.handleAdminTeams)
	mux.HandleFunc("/api/admin/players", srv.handleAdminPlayers)
	mux.HandleFunc("/api/teams", srv.handleTeams)

	// HTTP timeouts protect against slowloris on the JSON endpoints and the
	// WS upgrade handshake. WS connections clear the underlying socket
	// deadline in UpgradeHTTP so these limits do not kill long-lived game
	// connections after the upgrade completes.
	server := &http.Server{
		Addr:              ":9092",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Println("Ballerburg multiplayer server listening on :9092")
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func parseAllowedOrigins(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) handleScoreboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scoreboard == nil {
		http.Error(w, "scoreboard unavailable", http.StatusServiceUnavailable)
		return
	}
	snap, err := s.scoreboard.Snapshot()
	if err != nil {
		log.Printf("scoreboard: snapshot failed: %v", err)
		http.Error(w, "scoreboard error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		log.Printf("scoreboard: encode failed: %v", err)
	}
}
