package main

import "testing"

// TestPlaceBrickRejectsStackingOnBattlement guards the "two bricks on top of
// each other" bug: battlements are added with X = ltX+6+i*24 (i.e. NOT
// aligned to BS=10) and W=14 < BS. A click in the BS-aligned gap next to
// such a battlement used to fall through to PlaceBrick's else-branch and
// drop a fresh 1×1 wall whose [bx, bx+BS) AABB visually overlapped the
// battlement, producing two stacked bricks at the same screen position.
//
// After the fix, the engine refuses any new-wall placement whose candidate
// cell overlaps an alive brick in any existing wall and surfaces the same
// "brick already there" error as the in-grid duplicate case.
func TestPlaceBrickRejectsStackingOnBattlement(t *testing.T) {
	e := NewEng()
	cs := &e.GS.Castles[0]

	// Find a battlement whose X is not BS-aligned and is inside the build
	// area horizontally (battlements always are).
	var bat *Wall
	for i := range cs.Walls {
		w := &cs.Walls[i]
		if w.Kind != "battlement" {
			continue
		}
		if w.X%BS == 0 {
			continue
		}
		bat = w
		break
	}
	if bat == nil {
		t.Fatalf("expected a non-BS-aligned battlement in initial layout, got none")
	}

	// Choose a click coord that:
	//   * lies inside the build area horizontally (basex+4 .. basex+basew-4),
	//   * is NOT inside the battlement's bbox (so the clicked-branch misses
	//     and we fall through to the else-branch),
	//   * but whose BS-snapped cell [bx, bx+BS) overlaps the battlement.
	// bx = floor(x/BS)*BS, so we want bx s.t. bx < bat.X+bat.W && bx+BS > bat.X
	// and the click is outside [bat.X, bat.X+bat.W).
	bx := (bat.X / BS) * BS // round down — sits just left of the battlement
	if bx+BS <= bat.X {
		t.Fatalf("test setup: expected bx=%d to overlap battlement [%d,%d)",
			bx, bat.X, bat.X+bat.W)
	}
	x := bx + 1                  // strictly inside [bx, bx+BS) and below bat.X
	y := bat.Y + bat.CellH/2 + 1 // inside the battlement's Y band

	// Make sure x lies outside the battlement's X span so the clicked-loop
	// genuinely misses (otherwise we'd be testing the in-grid duplicate path).
	if x >= bat.X && x < bat.X+bat.W {
		t.Fatalf("test setup: click x=%d landed inside battlement bbox [%d,%d) — "+
			"need to pick an x outside it", x, bat.X, bat.X+bat.W)
	}

	// Pre-conditions for PlaceBrick to even consider the click.
	cs.PendingBricks = 5
	e.GS.Phase = "aim"
	e.GS.Turn = 0

	preWalls := len(cs.Walls)
	preBricks := cs.PendingBricks

	err := e.PlaceBrick(0, x, y)
	if err == nil {
		t.Fatalf("PlaceBrick(%d,%d) succeeded — expected stacking rejection. "+
			"Walls grew %d→%d, pending %d→%d",
			x, y, preWalls, len(cs.Walls), preBricks, cs.PendingBricks)
	}
	if err.Error() != "brick already there" {
		t.Fatalf("unexpected error message %q (want %q)", err.Error(),
			"brick already there")
	}
	if len(cs.Walls) != preWalls {
		t.Fatalf("Walls slice grew %d→%d on a rejected placement",
			preWalls, len(cs.Walls))
	}
	if cs.PendingBricks != preBricks {
		t.Fatalf("PendingBricks changed %d→%d on a rejected placement",
			preBricks, cs.PendingBricks)
	}
}

// TestPlaceBrickAllowsAdjacentToBattlement is the companion to the stacking
// guard: the user must still be able to place a brick in a BS cell that
// merely TOUCHES a battlement (shares an edge) without overlapping any of
// its alive grid cells. Without this assertion the stacking-rejection above
// could be implemented as "reject everything near a non-aligned wall",
// which would block all legal placements next to battlements.
//
// Strategy: walk every BS-aligned cell that touches a non-aligned battlement
// but doesn't overlap any alive brick, and stop on the first one inside the
// build area with a live 4-neighbour. We expect at least one such cell to
// exist in any random layout — battlements are flanked by towers and keeps,
// which always provide live neighbours along their shared edges.
func TestPlaceBrickAllowsAdjacentToBattlement(t *testing.T) {
	e := NewEng()
	cs := &e.GS.Castles[0]

	upperLimit := cs.BuildTopY - BS*10
	if upperLimit < 0 {
		upperLimit = 0
	}
	leftBound := cs.BaseX + 4
	rightBound := cs.BaseX + cs.BaseW - 4

	hasLiveNb := func(bx, by int) bool {
		cx := bx + BS/2
		cy := by + BS/2
		offs := [4][2]int{{-BS, 0}, {BS, 0}, {0, -BS}, {0, BS}}
		for _, o := range offs {
			for i := range cs.Walls {
				if hitW(cs.Walls[i], cx+o[0], cy+o[1]) {
					return true
				}
			}
		}
		return false
	}

	type cand struct{ bx, by int }
	var picked *cand
	for i := range cs.Walls {
		w := &cs.Walls[i]
		if w.Kind != "battlement" || w.X%BS == 0 {
			continue
		}
		// Try BS-aligned cells along all 4 sides of this battlement.
		ybands := []int{
			((w.Y - BS) / BS) * BS,
			(w.Y / BS) * BS,
			((w.Y + w.H - 1) / BS) * BS,
			((w.Y + w.H) / BS) * BS,
		}
		xbands := []int{
			((w.X - BS) / BS) * BS,
			((w.X + w.W) / BS) * BS,
		}
		var trials []cand
		for _, bx := range xbands {
			for _, by := range ybands {
				trials = append(trials, cand{bx, by})
			}
		}
		for _, c := range trials {
			if c.bx < leftBound || c.bx+BS > rightBound {
				continue
			}
			if c.by < upperLimit || c.by+BS > cs.GroundPx {
				continue
			}
			if cellOverlapsAliveBrick(cs.Walls, c.bx, c.by, BS, BS) {
				continue
			}
			if !hasLiveNb(c.bx, c.by) {
				continue
			}
			cc := c
			picked = &cc
			break
		}
		if picked != nil {
			break
		}
	}
	if picked == nil {
		t.Skip("no adjacent-to-battlement BS cell with a live neighbour found in this layout")
	}

	cs.PendingBricks = 5
	e.GS.Phase = "aim"
	e.GS.Turn = 0
	preWalls := len(cs.Walls)
	preBricks := cs.PendingBricks

	if err := e.PlaceBrick(0, picked.bx+BS/2, picked.by+BS/2); err != nil {
		t.Fatalf("PlaceBrick at non-overlapping cell adjacent to battlement "+
			"unexpectedly failed: %v (bx=%d, by=%d)", err, picked.bx, picked.by)
	}
	if len(cs.Walls) != preWalls+1 {
		t.Fatalf("expected exactly one new wall, walls %d→%d", preWalls, len(cs.Walls))
	}
	if cs.PendingBricks != preBricks-1 {
		t.Fatalf("expected PendingBricks to drop by 1, %d→%d", preBricks, cs.PendingBricks)
	}
}
