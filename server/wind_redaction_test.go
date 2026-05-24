package main

import (
	"math"
	"testing"
)

func TestStateForPlayerKeepsExactWindWithOwnVane(t *testing.T) {
	l := &Lobby{eng: NewEng()}
	l.eng.GS.Wind = 1.24
	l.eng.GS.Castles[0].WindVane.Alive = true
	l.eng.GS.Castles[0].WindVane.Owned = true

	v := l.stateForPlayer(0)
	if !v.WindExact {
		t.Fatalf("expected exact wind for player 0")
	}
	if v.Wind != l.eng.GS.Wind {
		t.Fatalf("expected exact wind %.4f, got %.4f", l.eng.GS.Wind, v.Wind)
	}
}

func TestWindRedactionSmoke(t *testing.T) {
	l := &Lobby{eng: NewEng()}
	trueWind := 1.4
	l.eng.GS.Wind = trueWind
	l.eng.GS.Castles[0].WindVane.Alive = false
	l.eng.GS.Castles[0].WindVane.Owned = true
	l.eng.GS.Castles[1].WindVane.Alive = true
	l.eng.GS.Castles[1].WindVane.Owned = true

	p0a := l.stateForPlayer(0)
	p0b := l.stateForPlayer(0)
	if p0a.WindExact || p0b.WindExact {
		t.Fatalf("expected redacted wind for player 0")
	}
	if p0a.Wind != p0b.Wind {
		t.Fatalf("expected stable redacted wind within one turn, got %.4f then %.4f", p0a.Wind, p0b.Wind)
	}
	if p0a.Wind == trueWind {
		t.Fatalf("expected redacted wind to differ from exact wind %.4f", trueWind)
	}
	ratio := math.Abs(p0a.Wind / trueWind)
	if ratio < 0.5 || ratio >= 1.0 {
		t.Fatalf("expected redacted wind ratio in [0.5,1.0), got %.4f", ratio)
	}

	p1 := l.stateForPlayer(1)
	if !p1.WindExact {
		t.Fatalf("expected exact wind for player 1")
	}
	if p1.Wind != trueWind {
		t.Fatalf("expected player 1 exact wind %.4f, got %.4f", trueWind, p1.Wind)
	}
}

func TestStateForPlayerRefreshesRedactionAcrossTurns(t *testing.T) {
	l := &Lobby{eng: NewEng()}
	l.eng.GS.Castles[0].WindVane.Alive = false
	l.eng.GS.Castles[0].WindVane.Owned = true

	l.eng.GS.Wind = -1.2
	first := l.stateForPlayer(0)
	if first.WindExact {
		t.Fatalf("expected first state to be redacted")
	}

	l.eng.GS.Round++
	l.eng.GS.Turn = 1 - l.eng.GS.Turn
	l.eng.GS.Wind = 0.9
	next := l.stateForPlayer(0)
	nextAgain := l.stateForPlayer(0)
	if next.WindExact {
		t.Fatalf("expected next state to be redacted")
	}
	if next.Wind != nextAgain.Wind {
		t.Fatalf("expected stable redacted wind after turn change, got %.4f then %.4f", next.Wind, nextAgain.Wind)
	}
	if math.Signbit(next.Wind) != math.Signbit(l.eng.GS.Wind) {
		t.Fatalf("expected redacted wind sign to match exact wind sign")
	}
}
