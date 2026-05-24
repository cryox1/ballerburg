package main

import (
	"testing"
	"time"
)

func TestAICmdDelaySpeedsBrickActions(t *testing.T) {
	defaultHard := aiCmdDelay("hard", Cmd{Action: "fire"})
	if defaultHard != 1400*time.Millisecond {
		t.Fatalf("hard default delay changed unexpectedly: %v", defaultHard)
	}

	placeDelay := aiCmdDelay("hard", Cmd{Action: "place_brick"})
	if placeDelay != 120*time.Millisecond {
		t.Fatalf("unexpected place_brick delay: %v", placeDelay)
	}
	if placeDelay >= defaultHard {
		t.Fatalf("place_brick delay must be faster than default hard delay")
	}

	buyBricksDelay := aiCmdDelay("hard", Cmd{Action: "buy", Payload: BuyP{Item: "buy-bricks"}})
	if buyBricksDelay != 220*time.Millisecond {
		t.Fatalf("unexpected buy-bricks delay: %v", buyBricksDelay)
	}
	if buyBricksDelay >= defaultHard {
		t.Fatalf("buy-bricks delay must be faster than default hard delay")
	}
	if buyBricksDelay <= placeDelay {
		t.Fatalf("buy-bricks delay should stay slightly above place_brick delay")
	}
}
