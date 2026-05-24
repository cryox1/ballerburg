package main

import "testing"

func TestSurrenderEndsGame(t *testing.T) {
	e := NewEng()
	e.GS.Phase = "aim"
	e.GS.Turn = 0

	if err := e.Surrender(0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.GS.Phase != "gameover" {
		t.Fatalf("expected phase=gameover, got %q", e.GS.Phase)
	}
	if e.GS.Winner == nil {
		t.Fatal("expected winner to be set")
	}
	if *e.GS.Winner != 1 {
		t.Fatalf("expected winner=1 (opponent), got %d", *e.GS.Winner)
	}
	if e.GS.Reason == "" {
		t.Fatal("expected reason string to be set")
	}
}

func TestSurrenderRejectedOutsideAimPhase(t *testing.T) {
	e := NewEng()
	e.GS.Phase = "flying"

	if err := e.Surrender(0); err == nil {
		t.Fatal("expected error when surrendering during flying phase")
	}
	if e.GS.Phase != "flying" {
		t.Fatalf("phase should be unchanged, got %q", e.GS.Phase)
	}
}

func TestSurrenderRejectedWhenGameAlreadyOver(t *testing.T) {
	e := NewEng()
	e.GS.Phase = "gameover"
	w := 1
	e.GS.Winner = &w

	if err := e.Surrender(0); err == nil {
		t.Fatal("expected error when surrendering after game over")
	}
}
