package main

import "testing"

// hasBrickAt returns true if any alive wall has an alive brick covering (x,y).
func hasBrickAt(walls []Wall, x, y int) bool {
	for _, w := range walls {
		if w.HP <= 0 {
			continue
		}
		if x < w.X || x >= w.X+w.W || y < w.Y || y >= w.Y+w.H {
			continue
		}
		rx := (x - w.X) / w.CellW
		ry := (y - w.Y) / w.CellH
		if rx < 0 {
			rx = 0
		}
		if rx >= w.Cols {
			rx = w.Cols - 1
		}
		if ry < 0 {
			ry = 0
		}
		if ry >= w.Rows {
			ry = w.Rows - 1
		}
		if w.Bricks[ry*w.Cols+rx] == 1 {
			return true
		}
	}
	return false
}

// countRow counts alive bricks at row top y across [xs, xe).
func countRow(walls []Wall, xs, xe, y int) int {
	n := 0
	for x := xs; x < xe; x += BS {
		if hasBrickAt(walls, x, y) {
			n++
		}
	}
	return n
}

// TestBuyCannonRestoresSupportRow simulates a destroyed cannon (Alive=false +
// every brick beneath it carved away, including the flanking-tower wall set
// to HP=0). After Buy("buy-cannon") the cannon should be alive AND a row of
// bricks at flankY=cn.BY+28 spanning the flanking-tower width should exist
// so cannonSupported() finds at least one brick.
func TestBuyCannonRestoresSupportRow(t *testing.T) {
	e := NewEng()
	cs := &e.GS.Castles[0]
	cn := &cs.Cannons[0]
	flankY := cn.BY + 28

	// Sanity: pre-destroy, the flanking tower has bricks at the cannon's BX
	// at flankY.
	preCount := countRow(cs.Walls, cn.BX-LT_W/2, cn.BX+LT_W/2, flankY)
	if preCount == 0 {
		t.Fatalf("expected initial bricks at flankY=%d under cannon BX=%d, got 0", flankY, cn.BX)
	}

	// Simulate destruction: kill the cannon and zero out every wall whose
	// area overlaps the cannon's support-check column. This mimics the
	// damage from a direct hit + nearby explosions.
	cn.Alive = false
	for i := range cs.Walls {
		w := &cs.Walls[i]
		// If wall overlaps the cannon's column [BX-LT_W/2, BX+LT_W/2] and
		// the row at flankY..flankY+BS, blank it.
		if w.X+w.W <= cn.BX-LT_W/2 || w.X >= cn.BX+LT_W/2 {
			continue
		}
		if w.Y+w.H <= flankY-30 || w.Y >= flankY+60 {
			continue
		}
		// Zero out all bricks; set HP to 0 so hitW returns false.
		for k := range w.Bricks {
			w.Bricks[k] = 0
		}
		w.HP = 0
	}

	postDestroyCount := countRow(cs.Walls, cn.BX-LT_W/2, cn.BX+LT_W/2, flankY)
	if postDestroyCount != 0 {
		t.Fatalf("expected 0 bricks after manual destroy, got %d", postDestroyCount)
	}
	if cannonSupported(cs, cn.BX, cn.BY) {
		t.Fatalf("expected cannonSupported=false after destroy")
	}

	// Force turn so Buy() accepts.
	e.GS.Turn = 0
	if err := e.Buy(0, "buy-cannon"); err != nil {
		t.Fatalf("Buy buy-cannon failed: %v", err)
	}

	if !cs.Cannons[0].Alive {
		t.Fatalf("cannon not revived after Buy")
	}
	postBuyCount := countRow(cs.Walls, cn.BX-LT_W/2, cn.BX+LT_W/2, flankY)
	if postBuyCount == 0 {
		t.Fatalf("expected support row after Buy, got 0 bricks at flankY=%d", flankY)
	}
	if !cannonSupported(cs, cn.BX, cn.BY) {
		t.Fatalf("cannonSupported=false after Buy — cannon still floating!")
	}
	t.Logf("cannon repaired with %d support bricks at flankY=%d", postBuyCount, flankY)
}

func TestBuyWindvaneRestoresSupportRow(t *testing.T) {
	e := NewEng()
	cs := &e.GS.Castles[0]

	towerY := cs.WindVane.TowerY
	vx := cs.WindVane.X
	preCount := countRow(cs.Walls, vx-VANE_TW_W/2, vx+VANE_TW_W/2, towerY)
	if preCount == 0 {
		t.Fatalf("expected initial bricks at vane TowerY=%d, got 0", towerY)
	}

	// Simulate vane-tower destruction: zero out vane-tower walls + flag.
	cs.WindVane.Alive = false
	for i := range cs.Walls {
		w := &cs.Walls[i]
		if w.Kind != "vane-tower" {
			continue
		}
		for k := range w.Bricks {
			w.Bricks[k] = 0
		}
		w.HP = 0
	}

	postDestroyCount := countRow(cs.Walls, vx-VANE_TW_W/2, vx+VANE_TW_W/2, towerY)
	if postDestroyCount != 0 {
		t.Fatalf("expected 0 vane bricks after destroy, got %d", postDestroyCount)
	}

	e.GS.Turn = 0
	if err := e.Buy(0, "buy-windvane"); err != nil {
		t.Fatalf("Buy buy-windvane failed: %v", err)
	}
	if !cs.WindVane.Alive {
		t.Fatalf("windvane not revived after Buy")
	}
	postBuyCount := countRow(cs.Walls, vx-VANE_TW_W/2, vx+VANE_TW_W/2, towerY)
	if postBuyCount == 0 {
		t.Fatalf("expected support row at vane TowerY=%d, got 0 bricks", towerY)
	}
	// The whole vane-tower must be rebuilt, not just the top row — otherwise
	// the flag floats on a sliver and the floating-vane cascade removes it.
	lowerY := towerY + VANE_TW_H - BS
	lowerCount := countRow(cs.Walls, vx-VANE_TW_W/2, vx+VANE_TW_W/2, lowerY)
	if lowerCount == 0 {
		t.Fatalf("expected rebuilt bricks at lower vane-tower row y=%d, got 0 — flag would float", lowerY)
	}
	t.Logf("windvane repaired: %d bricks at TowerY=%d, %d at lowerY=%d", postBuyCount, towerY, lowerCount, lowerY)
}
