package main

import "testing"

func prepMediumAIBrickDecision(gs *GS, slot int) {
	cs := &gs.Castles[slot]
	cs.Gold = 420
	cs.Powder = 200
	cs.Ammo = 10
	cs.ShrapnelAmmo = 0
	cs.PendingBricks = 0
	cs.WindVane.Alive = true
	cs.Towers = []Twr{
		{X: 0, Y: 0, W: 1, H: 1, HP: 100, HPMax: 100},
		{X: 0, Y: 0, W: 1, H: 1, HP: 100, HPMax: 100},
	}
	for i := range cs.Cannons {
		cs.Cannons[i].Alive = true
	}
}

func cmdBuys(cmd Cmd, item string) bool {
	if cmd.Action != "buy" {
		return false
	}
	p, ok := cmd.Payload.(BuyP)
	if !ok {
		return false
	}
	return p.Item == item
}

func knockOutRepairBandBricks(cs *Castle, chamberKind string, n int) int {
	ch, ok := chamberByKind(cs, chamberKind)
	if !ok {
		return 0
	}
	bandTop := ch.Y - AI_REPAIR_CHAMBER_BAND
	removed := 0
	for i := range cs.Walls {
		w := &cs.Walls[i]
		if !wallEligibleForRepair(cs, w, chamberKind) {
			continue
		}
		for ry := 0; ry < w.Rows && removed < n; ry++ {
			for rx := 0; rx < w.Cols && removed < n; rx++ {
				idx := ry*w.Cols + rx
				if w.Bricks[idx] == 0 {
					continue
				}
				cx := w.X + rx*w.CellW + w.CellW/2
				cy := w.Y + ry*w.CellH + w.CellH/2
				if cx < ch.X || cx >= ch.X+ch.W || cy < bandTop || cy >= ch.Y {
					continue
				}
				w.Bricks[idx] = 0
				w.HP--
				removed++
			}
		}
	}
	return removed
}

func knockOutPowderUpperTowerBricks(cs *Castle, n int) int {
	ch, ok := chamberByKind(cs, "powder")
	if !ok {
		return 0
	}
	bandTop := ch.Y - AI_REPAIR_CHAMBER_BAND
	removed := 0
	for i := range cs.Walls {
		w := &cs.Walls[i]
		if w.Kind != "tower-out" {
			continue
		}
		if w.X+w.W <= ch.X || w.X >= ch.X+ch.W {
			continue
		}
		for ry := 0; ry < w.Rows && removed < n; ry++ {
			for rx := 0; rx < w.Cols && removed < n; rx++ {
				idx := ry*w.Cols + rx
				if w.Bricks[idx] == 0 {
					continue
				}
				cy := w.Y + ry*w.CellH + w.CellH/2
				if cy >= bandTop {
					continue
				}
				w.Bricks[idx] = 0
				w.HP--
				removed++
			}
		}
	}
	return removed
}

func TestComputeAICmdSkipsBricksOnMinorShieldDamage(t *testing.T) {
	e := NewEng()
	prepMediumAIBrickDecision(&e.GS, 1)
	cs := &e.GS.Castles[1]

	removed := knockOutRepairBandBricks(cs, "throne", 6)
	if removed != 6 {
		t.Fatalf("expected to remove 6 throne-shield bricks, removed=%d", removed)
	}
	if criticalWallsDamaged(cs) {
		t.Fatalf("minor throne damage incorrectly marked critical")
	}

	cmd := computeAICmd(&e.GS, 1, "medium")
	if cmdBuys(cmd, "buy-bricks") {
		t.Fatalf("AI bought bricks on minor chamber-shield damage")
	}
}

func TestComputeAICmdBuysBricksOnSevereShieldDamage(t *testing.T) {
	e := NewEng()
	prepMediumAIBrickDecision(&e.GS, 1)
	cs := &e.GS.Castles[1]

	removed := knockOutRepairBandBricks(cs, "treasure", 48)
	if removed < 40 {
		t.Fatalf("failed to create severe treasure-shield damage: removed=%d", removed)
	}
	if !criticalWallsDamaged(cs) {
		t.Fatalf("severe treasure damage was not marked critical")
	}

	cmd := computeAICmd(&e.GS, 1, "medium")
	if !cmdBuys(cmd, "buy-bricks") {
		t.Fatalf("expected buy-bricks on severe chamber-shield damage, got action=%q payload=%#v", cmd.Action, cmd.Payload)
	}
}

func TestComputeAICmdIgnoresCannonTowerSuperstructureDamage(t *testing.T) {
	e := NewEng()
	prepMediumAIBrickDecision(&e.GS, 1)
	cs := &e.GS.Castles[1]

	removed := knockOutPowderUpperTowerBricks(cs, 60)
	if removed < 40 {
		t.Fatalf("failed to create upper-tower damage: removed=%d", removed)
	}
	if criticalWallsDamaged(cs) {
		t.Fatalf("upper cannon-tower damage incorrectly marked critical")
	}
	if _, _, ok := findRepairCell(cs); ok {
		t.Fatalf("findRepairCell proposed a repair inside cannon-tower superstructure")
	}

	cmd := computeAICmd(&e.GS, 1, "medium")
	if cmdBuys(cmd, "buy-bricks") {
		t.Fatalf("AI bought bricks for cannon-tower superstructure damage")
	}
}
