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

	srv := newServer(sb)
	http.HandleFunc("/ws", srv.handleWS)
	http.HandleFunc("/scoreboard", srv.handleScoreboard)
	http.HandleFunc("/api/admin", srv.handleAdmin)

	log.Println("Ballerburg multiplayer server listening on :9092")
	if err := http.ListenAndServe(":9092", nil); err != nil {
		log.Fatal(err)
	}
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
