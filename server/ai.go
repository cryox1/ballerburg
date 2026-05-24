// Copyright 2026 Jonas Bartel
// SPDX-License-Identifier: Apache-2.0

package main

// Server-side AI opponent for single-player mode.
//
// computeAICmd is invoked synchronously inside Lobby.loop() (which owns the
// engine's GS) and returns either a "fire" or "end_turn" command. The lobby
// then schedules its delivery onto the cmds channel via time.AfterFunc, so the
// AI's command flows through the same code path as a human's.

import (
	"log"
	"math"
	"math/rand"
	"os"
)

// ─── Tunables ───────────────────────────────────────────────────────────────
// Hard-AI knobs grouped here so behavior tuning stays local.
const (
	// Liquid gold the AI keeps in reserve when revival is plausibly needed.
	AI_HARD_CANNON_RESERVE = PRICE_CANNON // 260

	// Tower buy gates. 1st tower may be bought as soon as post-buy gold ≥ reserve.
	// 2nd tower waits for round + a stricter reserve so the AI doesn't strand
	// itself like the original bug.
	AI_HARD_TOWER1_POST_BUY_GOLD = AI_HARD_CANNON_RESERVE      // need ≥ 480 gold
	AI_HARD_TOWER2_MIN_ROUND     = 4
	AI_HARD_TOWER2_POST_BUY_GOLD = AI_HARD_CANNON_RESERVE + 80 // need ≥ 560 gold

	// Schrapnel use is gated on opp having ≥2 alive cannons (where spread shines).
	AI_HARD_SHRAPNEL_MAX_STOCK       = 2
	AI_HARD_SHRAPNEL_OPP_CANNONS_MIN = 2

	// Wind Monte Carlo: perturb gs.Wind per (angle, power) candidate so the AI
	// picks shots that survive mid-flight gusts, not just nominal wind.
	AI_HARD_WIND_SAMPLES = 5
	AI_HARD_WIND_SIGMA   = 0.30

	// Adaptive aggression thresholds on pressureScore ∈ [-1,+1].
	AI_HARD_PRESSURE_AGGR_THRESHOLD = 0.25
	AI_HARD_PRESSURE_DEF_THRESHOLD  = -0.25

	// Easy/Medium reserve floors before optional buys (towers, etc).
	AI_EASY_RESERVE   = PRICE_CANNON / 2 // 130
	AI_MEDIUM_RESERVE = PRICE_CANNON     // 260

	// Wall-repair policy for medium/hard AI.
	// Only chamber shields are maintained (throne/treasure/powder), never
	// cannon-tower superstructure. Repairs trigger only once damage is severe.
	AI_REPAIR_CHAMBER_BAND        = 140
	AI_REPAIR_TRIGGER_MIN_MISSING = BRICK_PACK_AMOUNT
	AI_REPAIR_TRIGGER_RATIO       = 0.15
)

var aiRepairPriority = [...]string{"throne", "treasure", "powder"}

// computeAICmd reads gs (it must NOT mutate it) and returns the AI's next
// command for player slot `slot` at difficulty `diff`. The lobby loop calls
// this after every state mutation, so returning a "buy" or "place_brick"
// command does NOT end the turn — the AI will be re-asked and can chain
// multiple maintenance actions before finally firing.
//
// Priority order (top wins):
//  1. Refill critically-low powder.
//  2. Refill empty ammo.
//  3. Revive a destroyed cannon if affordable.
//  3.5 Hard: sell a tower as recovery when broke + cannon dead (refunds 110 →
//      eventually triggers step 3 again).
//  4. Hard only: dynamic tax management.
//  5. Medium/hard: rebuild wind-vane (reserve-aware).
//  6. Hard: buy Schrapnell when opp has ≥2 alive cannons (reserve-aware).
//  7. Medium/hard: buy Förderturm (reserve- and round-gated — fixes the bug
//     where Hard burned gold on tower #2 and couldn't replace cannons).
//  8. Medium/hard: place a pending brick on a damaged wall.
//  9. Medium/hard: buy a brick pack (reserve-aware).
// 10. Fire (or end turn if truly unable to act).
func computeAICmd(gs *GS, slot int, diff string) Cmd {
	cs := &gs.Castles[slot]
	opp := &gs.Castles[1-slot]

	// 1. Powder refill — non-optional even if it dips below the reserve. A
	//    powder-starved AI can't fire, which is worse than running short on
	//    cannon-replacement money. Threshold is one full magazine (50): below
	//    that the next shot is power-clamped and likely falls short of the
	//    opponent, where it can land on our own castle (engine collision only
	//    exempts the firing cannon, not the rest of our structures).
	if cs.Powder < 50 && cs.Gold >= PRICE_POWDER*50 {
		return Cmd{Action: "buy", Payload: BuyP{Item: "buy-powder-50"}}
	}

	// 2. Ammo refill — same rationale as powder.
	if cs.Ammo <= 0 && cs.ShrapnelAmmo <= 0 && cs.Gold >= PRICE_AMMO*5 {
		return Cmd{Action: "buy", Payload: BuyP{Item: "buy-ammo-5"}}
	}

	// 3. Revive a destroyed cannon if affordable.
	if hasDeadCannonSlot(cs) && countAliveCannons(cs) < cs.MaxCannons && cs.Gold >= PRICE_CANNON {
		return Cmd{Action: "buy", Payload: BuyP{Item: "buy-cannon"}}
	}

	// 3.5 Hard-only sell-tower recovery. When a cannon is dead AND we can't
	//     afford a replacement AND we own at least one tower, refund the
	//     lowest-HP tower. The next call recomputes; once gold ≥ PRICE_CANNON
	//     step 3 fires, dead-slot clears, and the loop terminates.
	if diff == "hard" && hasDeadCannonSlot(cs) &&
		countAliveCannons(cs) < cs.MaxCannons &&
		cs.Gold < PRICE_CANNON && len(cs.Towers) > 0 {
		if idx, ok := lowestHPTowerIdx(cs); ok {
			return Cmd{Action: "sell_tower", Payload: SellTowerP{Index: idx}}
		}
	}

	// 4. Hard tax management.
	if diff == "hard" {
		broke := (cs.Ammo <= 0 && cs.Gold < PRICE_AMMO*5) ||
			(cs.Powder < 50 && cs.Gold < PRICE_POWDER*50) ||
			(hasDeadCannonSlot(cs) && cs.Gold < PRICE_CANNON)
		if broke && cs.TaxRate < 60 {
			return Cmd{Action: "set_tax", Payload: TaxP{Tax: 60}}
		}
		if cs.Gold > 500 && cs.TaxRate > 40 && !hasDeadCannonSlot(cs) {
			return Cmd{Action: "set_tax", Payload: TaxP{Tax: 40}}
		}
	}

	// 5–9. Medium/hard maintenance buys.
	if diff != "easy" {
		// 5. Wind-vane.
		if !cs.WindVane.Alive && canAffordWithReserve(cs, opp, gs.Round, PRICE_VANE) {
			return Cmd{Action: "buy", Payload: BuyP{Item: "buy-windvane"}}
		}
		// 6. Förderturm — towers compound income so they go before one-shot
		//    consumables. Round- and reserve-gated; this is the bug fix.
		if canBuyTower(cs, opp, gs.Round, diff) {
			return Cmd{Action: "buy", Payload: BuyP{Item: "buy-tower"}}
		}
		// 7. Schrapnel (Hard only). Gated on a live powder chamber so we don't
		//    waste 100 gold on a shot we can't fire (engine.Fire still requires
		//    cs.Powder >= power for shrapnel).
		if diff == "hard" &&
			countAliveCannons(opp) >= AI_HARD_SHRAPNEL_OPP_CANNONS_MIN &&
			cs.ShrapnelAmmo < AI_HARD_SHRAPNEL_MAX_STOCK &&
			canAffordWithReserve(cs, opp, gs.Round, PRICE_SHRAPNEL) &&
			chamberAlive(cs, "powder") && cs.Powder >= 25 {
			return Cmd{Action: "buy", Payload: BuyP{Item: "buy-shrapnel"}}
		}
		// 8. Brick placement.
		if cs.PendingBricks > 0 {
			if x, y, ok := findRepairCell(cs); ok {
				return Cmd{Action: "place_brick", Payload: PlaceBrickP{X: x, Y: y}}
			}
			// No valid placement anywhere — end turn to unblock rather than
			// looping forever (fire is rejected while PendingBricks > 0).
			return Cmd{Action: "end_turn"}
		}
		// 9. Brick pack (reserve-aware).
		if criticalWallsDamaged(cs) && canAffordWithReserve(cs, opp, gs.Round, PRICE_BRICK_PACK) {
			if _, _, ok := findRepairCell(cs); ok {
				return Cmd{Action: "buy", Payload: BuyP{Item: "buy-bricks"}}
			}
		}
	}

	// 10. Fire — find any alive cannon.
	alive := []int{}
	for i, cn := range cs.Cannons {
		if cn.Alive {
			alive = append(alive, i)
		}
	}
	if len(alive) == 0 {
		// Nothing alive and we couldn't afford a replacement; bank gold next turn.
		return Cmd{Action: "end_turn"}
	}

	// Out of all ammo and broke — must end turn so income can flow.
	if cs.Ammo <= 0 && cs.ShrapnelAmmo <= 0 {
		return Cmd{Action: "end_turn"}
	}

	maxPower := 50
	if cs.Powder < maxPower {
		maxPower = cs.Powder
	}
	// Below this floor the shot falls short and can land on our own castle
	// (engine.applyExplosion only exempts the firing cannon, not the rest of
	// our structures). End the turn so income builds up for a refill.
	if maxPower < 25 {
		return Cmd{Action: "end_turn"}
	}

	var cannon, angle, power int
	ammoType := ""
	switch diff {
	case "easy":
		cannon, angle, power = aiEasy(cs, opp, alive, maxPower, gs.PTOS)
	case "hard":
		cannon, angle, power, ammoType = aiHard(gs, cs, opp, slot, alive, maxPower)
	default: // "medium" and any unknown difficulty fall back to medium
		cannon, angle, power = aiMedium(cs, opp, alive, maxPower, gs.PTOS)
	}

	// If we picked normal ammo but have none, fall back to shrapnel (and vice
	// versa). Prevents fire-loop rejections after the maintenance phase.
	if ammoType == "" && cs.Ammo <= 0 && cs.ShrapnelAmmo > 0 {
		ammoType = "shrapnel"
	}
	if ammoType == "shrapnel" && cs.ShrapnelAmmo <= 0 {
		ammoType = ""
	}

	angle = clampInt(angle, 10, 89)
	power = clampInt(power, 5, maxPower)

	if os.Getenv("BB_AI_DEBUG") != "" {
		log.Printf("AI[%s] slot=%d cannon=%d angle=%d power=%d ammo=%q wind=%.2f",
			diff, slot, cannon, angle, power, ammoType, gs.Wind)
	}

	return Cmd{
		Action:  "fire",
		Payload: FireP{Cannon: cannon, Angle: angle, Power: power, AmmoType: ammoType},
	}
}

// ─── Reserve / pressure helpers ─────────────────────────────────────────────

// goldReserveNeeded captures the gold the AI must keep liquid before any
// optional purchase. Multi-turn planning lives here: as future obligations
// (cannon revive, ammo refill, tower upkeep) become visible the reserve grows.
func goldReserveNeeded(cs, opp *Castle, round int) int {
	reserve := 0
	// Cannon-replacement reserve when revival may be needed.
	if hasDeadCannonSlot(cs) || countAliveCannons(opp) >= 2 {
		reserve += PRICE_CANNON
	}
	// Near-future ammo refill if we're about to run out.
	if cs.Ammo <= 5 {
		reserve += PRICE_AMMO * 5
	}
	// Near-future powder refill if we're below half a stack.
	if cs.Powder < 100 {
		reserve += PRICE_POWDER * 50
	}
	// Two rounds of tower upkeep.
	at := 0
	for _, t := range cs.Towers {
		if t.HP > 0 {
			at++
		}
	}
	reserve += at * TOWER_UPKEEP * 2
	_ = round // currently unused; kept in signature for future round-scaled rules.
	return reserve
}

// canAffordWithReserve returns true when paying `cost` would leave at least
// goldReserveNeeded(...) liquid.
func canAffordWithReserve(cs, opp *Castle, round, cost int) bool {
	return cs.Gold-cost >= goldReserveNeeded(cs, opp, round)
}

// canBuyTower applies difficulty-specific reserve and round gates. The 2nd
// tower on Hard is delayed until the economy stabilises so the AI doesn't
// strand itself with no cannon-revival money — the reported bug.
func canBuyTower(cs, opp *Castle, round int, diff string) bool {
	if len(cs.Towers) >= MAX_TOWERS {
		return false
	}
	if cs.Gold < PRICE_TOWER {
		return false
	}
	postBuy := cs.Gold - PRICE_TOWER
	switch diff {
	case "hard":
		if len(cs.Towers) == 0 {
			return postBuy >= AI_HARD_TOWER1_POST_BUY_GOLD
		}
		return round >= AI_HARD_TOWER2_MIN_ROUND &&
			postBuy >= AI_HARD_TOWER2_POST_BUY_GOLD
	case "easy":
		return postBuy >= AI_EASY_RESERVE
	default: // medium
		return postBuy >= AI_MEDIUM_RESERVE
	}
}

func chamberAlive(cs *Castle, kind string) bool {
	for _, ch := range cs.Chambers {
		if ch.Kind == kind && ch.Alive {
			return true
		}
	}
	return false
}

func lowestHPTowerIdx(cs *Castle) (int, bool) {
	idx := -1
	minHP := math.MaxInt
	for i, t := range cs.Towers {
		if t.HP <= 0 {
			continue
		}
		if t.HP < minHP {
			minHP = t.HP
			idx = i
		}
	}
	if idx < 0 {
		return 0, false
	}
	return idx, true
}

// pressureScore expresses how comfortably the AI is winning. Range [-1, +1]:
// +1 means the AI is dominant (push for kill); -1 means it's behind (defend).
// Used to bias target weights in aiHard.
func pressureScore(cs, opp *Castle) float64 {
	gold := float64(cs.Gold-opp.Gold) / 600.0
	can := float64(countAliveCannons(cs)-countAliveCannons(opp)) / 2.0
	pop := float64(cs.Population-opp.Population) / 200.0

	king := 0.0
	if !opp.King.Alive {
		king += 0.5
	}
	if !cs.King.Alive {
		king -= 0.5
	}

	wallsCs := wallHPRatio(cs)
	wallsOpp := wallHPRatio(opp)

	score := 0.30*gold + 0.30*can + 0.15*pop + 0.15*(wallsCs-wallsOpp) + 0.10*king
	if score > 1 {
		score = 1
	}
	if score < -1 {
		score = -1
	}
	return score
}

func wallHPRatio(cs *Castle) float64 {
	hp, hpMax := 0, 0
	for _, w := range cs.Walls {
		if w.Kind == "battlement" || w.Kind == "vane-tower" {
			continue
		}
		hp += w.HP
		hpMax += w.HPMax
	}
	if hpMax == 0 {
		return 1
	}
	return float64(hp) / float64(hpMax)
}

// ─── Maintenance helpers ────────────────────────────────────────────────────

func hasDeadCannonSlot(cs *Castle) bool {
	for _, cn := range cs.Cannons {
		if !cn.Alive {
			return true
		}
	}
	return false
}

func countAliveCannons(cs *Castle) int {
	n := 0
	for _, cn := range cs.Cannons {
		if cn.Alive {
			n++
		}
	}
	return n
}

// criticalWallsDamaged returns true only when a chamber shield is heavily
// damaged. The AI repairs in priority order: throne, then treasure, then powder.
// Cannon-tower superstructure is ignored on purpose to avoid wasteful spending.
func criticalWallsDamaged(cs *Castle) bool {
	for _, chamberKind := range aiRepairPriority {
		missing, total := chamberShieldDamage(cs, chamberKind)
		if total <= 0 {
			continue
		}
		need := chamberRepairThreshold(total)
		if missing >= need {
			return true
		}
	}
	return false
}

func chamberRepairThreshold(total int) int {
	need := int(math.Ceil(float64(total) * AI_REPAIR_TRIGGER_RATIO))
	if need < AI_REPAIR_TRIGGER_MIN_MISSING {
		need = AI_REPAIR_TRIGGER_MIN_MISSING
	}
	if need > total {
		need = total
	}
	return need
}

func chamberShieldDamage(cs *Castle, chamberKind string) (missing, total int) {
	ch, ok := chamberByKind(cs, chamberKind)
	if !ok {
		return 0, 0
	}
	bandTop := ch.Y - AI_REPAIR_CHAMBER_BAND
	for i := range cs.Walls {
		w := &cs.Walls[i]
		if !wallEligibleForRepair(cs, w, chamberKind) {
			continue
		}
		for ry := 0; ry < w.Rows; ry++ {
			for rx := 0; rx < w.Cols; rx++ {
				cx := w.X + rx*w.CellW + w.CellW/2
				cy := w.Y + ry*w.CellH + w.CellH/2
				if cx < ch.X || cx >= ch.X+ch.W || cy < bandTop || cy >= ch.Y {
					continue
				}
				total++
				if w.Bricks[ry*w.Cols+rx] == 0 {
					missing++
				}
			}
		}
	}
	return missing, total
}

func chamberByKind(cs *Castle, kind string) (Chamber, bool) {
	for i := range cs.Chambers {
		if cs.Chambers[i].Kind == kind {
			return cs.Chambers[i], true
		}
	}
	return Chamber{}, false
}

func wallOverlapsChamberBand(cs *Castle, w *Wall, chamberKind string) bool {
	ch, ok := chamberByKind(cs, chamberKind)
	if !ok {
		return false
	}
	if w.X+w.W <= ch.X || w.X >= ch.X+ch.W {
		return false
	}
	bandTop := ch.Y - AI_REPAIR_CHAMBER_BAND
	if w.Y+w.H <= bandTop || w.Y >= ch.Y {
		return false
	}
	return true
}

func wallEligibleForRepair(cs *Castle, w *Wall, chamberKind string) bool {
	switch chamberKind {
	case "throne":
		if w.Kind != "throne-wall" && w.Kind != "keep-wall" {
			return false
		}
	case "treasure":
		if w.Kind != "keep-wall" {
			return false
		}
	case "powder":
		if w.Kind != "tower-out" {
			return false
		}
	default:
		return false
	}
	return wallOverlapsChamberBand(cs, w, chamberKind)
}

// findRepairCell returns the centre pixel of an empty brick cell that
// engine.PlaceBrick accepts (must have a live neighbour). Repairs are limited
// to chamber shields above throne/treasure/powder; cannon-tower superstructure,
// partition walls, battlements, and vane-tower are excluded.
// Mirrors the neighbour rule in engine.PlaceBrick (engine.go:706-718) using
// BS-pixel offsets and hitW so we never propose a placement the engine rejects.
func findRepairCell(cs *Castle) (int, int, bool) {
	for _, chamberKind := range aiRepairPriority {
		for i := range cs.Walls {
			w := &cs.Walls[i]
			if w.HP >= w.HPMax || !wallEligibleForRepair(cs, w, chamberKind) {
				continue
			}
			if x, y, ok := emptyCellWithNeighbor(cs, w, chamberKind); ok {
				return x, y, true
			}
		}
	}
	return 0, 0, false
}

func emptyCellWithNeighbor(cs *Castle, w *Wall, chamberKind string) (int, int, bool) {
	ch, ok := chamberByKind(cs, chamberKind)
	if !ok {
		return 0, 0, false
	}
	bandTop := ch.Y - AI_REPAIR_CHAMBER_BAND
	for ry := 0; ry < w.Rows; ry++ {
		for rx := 0; rx < w.Cols; rx++ {
			idx := ry*w.Cols + rx
			if w.Bricks[idx] != 0 {
				continue
			}
			cx := w.X + rx*w.CellW + w.CellW/2
			cy := w.Y + ry*w.CellH + w.CellH/2
			if cx < ch.X || cx >= ch.X+ch.W || cy < bandTop || cy >= ch.Y {
				continue
			}
			if hasLiveNeighbor(cs, cx, cy) {
				return cx, cy, true
			}
		}
	}
	return 0, 0, false
}

func hasLiveNeighbor(cs *Castle, cx, cy int) bool {
	offs := [4][2]int{{-BS, 0}, {BS, 0}, {0, -BS}, {0, BS}}
	for _, o := range offs {
		nx := cx + o[0]
		ny := cy + o[1]
		for i := range cs.Walls {
			if hitW(cs.Walls[i], nx, ny) {
				return true
			}
		}
	}
	return false
}

// ─── Easy ───────────────────────────────────────────────────────────────────
// Reuse Medium's ballistic solution but add big gaussian noise. The AI now
// faces the opponent (so it stops shooting backwards) but routinely misses.

func aiEasy(cs, opp *Castle, alive []int, maxPower int, ptos float64) (int, int, int) {
	cannon, angle, power := aiMedium(cs, opp, alive, maxPower, ptos)
	angle += int(math.Round(rand.NormFloat64() * 18))
	power += int(math.Round(rand.NormFloat64() * 10))
	return cannon, angle, power
}

// ─── Medium ─────────────────────────────────────────────────────────────────
// Closed-form ballistic: pick a 45° (or 60° fallback) trajectory and solve for
// the velocity that lands on the opponent's keep center. Wind is ignored on
// purpose — players see the AI miss when wind is strong.

func aiMedium(cs *Castle, opp *Castle, alive []int, maxPower int, ptos float64) (int, int, int) {
	// Pick the front-most alive cannon (smallest forward distance to opponent).
	cannon := alive[0]
	bestDist := math.MaxFloat64
	for _, i := range alive {
		cn := cs.Cannons[i]
		fx := float64(cn.BX) + float64(cn.Side)*18
		dist := math.Abs(float64(opp.BaseX+BASE_W/2) - fx)
		if dist < bestDist {
			bestDist = dist
			cannon = i
		}
	}

	cn := cs.Cannons[cannon]
	dir := float64(cn.Side)
	x0 := float64(cn.BX) + dir*18
	y0 := float64(cn.BY) - 8

	// Target opponent keep, slightly above ground inside the keep.
	xT := float64(opp.BaseX + BASE_W/2)
	yT := float64(opp.GroundPx) - 100.0

	D := (xT - x0) * dir // forward distance
	drop := yT - y0      // canvas-y, positive if target lower than muzzle

	angleDeg, v := solveBallistic(D, drop, 45.0)
	if v <= 0 {
		// Target above muzzle / unreachable at 45°: try a higher arc.
		angleDeg, v = solveBallistic(D, drop, 60.0)
	}
	if v <= 0 {
		// Last-ditch: fall back to defaults.
		angleDeg, v = 55.0, float64(maxPower)*ptos
	}

	power := int(math.Round(v / ptos))
	angle := int(math.Round(angleDeg))

	// Inject realistic noise so the AI doesn't shoot identically each turn.
	angle += int(math.Round(rand.NormFloat64() * 8))
	power += int(math.Round(rand.NormFloat64() * 4))

	return cannon, angle, power
}

// solveBallistic returns the angle (unchanged from input) and required muzzle
// velocity v to land at forward distance D and vertical drop `drop` (canvas-y,
// positive = below muzzle). Returns v=0 if the target is unreachable at this
// angle.
func solveBallistic(D, drop, angleDeg float64) (float64, float64) {
	if D <= 0 {
		return angleDeg, 0
	}
	rad := angleDeg * math.Pi / 180
	cosT := math.Cos(rad)
	tanT := math.Tan(rad)
	denom := 2 * cosT * cosT * (D*tanT + drop)
	if denom <= 0 {
		return angleDeg, 0
	}
	v2 := GRAVITY * D * D / denom
	if v2 <= 0 {
		return angleDeg, 0
	}
	return angleDeg, math.Sqrt(v2)
}

// ─── Hard ───────────────────────────────────────────────────────────────────
// Grid-search (angle, power) per alive cannon, scoring each candidate by its
// expected value averaged over a few wind perturbations (Monte Carlo). Target
// weights bias toward kill-shots when ahead and threat-removal when behind.
// If shrapnel is in stock and the opponent has multiple alive cannons, score
// a second pass with a wider hit radius and pick whichever ammo wins.

type aiTarget struct {
	x, y   float64
	weight float64 // higher = more desirable
	kind   string  // "cannon" | "throne" | "tower" | "vane" | "keep"
}

// aiHard returns the chosen cannon, angle, power, and ammoType ("" or
// "shrapnel"). The ammoType lets the caller route to the right firing path.
func aiHard(gs *GS, cs *Castle, opp *Castle, slot int, alive []int, maxPower int) (int, int, int, string) {
	pressure := pressureScore(cs, opp)
	targets := collectTargets(opp, pressure)
	if len(targets) == 0 {
		c, a, p := aiMedium(cs, opp, alive, maxPower, gs.PTOS)
		return c, a, p, ""
	}

	maxP := maxPower
	if maxP > 50 {
		maxP = 50
	}

	bestCannon := alive[0]
	bestAngle := 45
	bestPower := clampInt(30, 5, maxP)
	bestAmmo := ""
	bestScore := -math.MaxFloat64

	canShrapnel := cs.ShrapnelAmmo > 0 &&
		countAliveCannons(opp) >= AI_HARD_SHRAPNEL_OPP_CANNONS_MIN &&
		chamberAlive(cs, "powder")

	for _, ci := range alive {
		cn := cs.Cannons[ci]
		dir := float64(cn.Side)
		x0 := float64(cn.BX) + dir*18
		y0 := float64(cn.BY) - 8

		for ang := 15; ang <= 85; ang += 5 {
			for pwr := 10; pwr <= maxP; pwr += 5 {
				// Normal pass.
				if cs.Ammo > 0 {
					s := avgScoreOverWind(gs, slot, x0, y0, dir,
						float64(ang), float64(pwr), targets, false)
					if s > bestScore {
						bestScore = s
						bestCannon = ci
						bestAngle = ang
						bestPower = pwr
						bestAmmo = ""
					}
				}
				// Shrapnel pass — wider hit radius, fixed penalty so a clean
				// normal hit still wins ties.
				if canShrapnel {
					s := avgScoreOverWind(gs, slot, x0, y0, dir,
						float64(ang), float64(pwr), targets, true) - 20.0
					if s > bestScore {
						bestScore = s
						bestCannon = ci
						bestAngle = ang
						bestPower = pwr
						bestAmmo = "shrapnel"
					}
				}
			}
		}
	}

	// Tiny noise so the AI doesn't fire identical shots.
	bestAngle += int(math.Round(rand.NormFloat64() * 2))
	bestPower += int(math.Round(rand.NormFloat64() * 2))
	return bestCannon, bestAngle, bestPower, bestAmmo
}

// avgScoreOverWind averages best-target scores across a few wind perturbations
// so the AI prefers shots that survive mid-flight gusts. When shrapnel is true,
// the hit radius is widened to SHRAPNEL_BLAST_R+16 and only cannons score
// (shrapnel's value comes from cannon-killing).
func avgScoreOverWind(gs *GS, slot int, x0, y0, dir, ang, pwr float64,
	targets []aiTarget, shrapnel bool) float64 {

	sum := 0.0
	for s := 0; s < AI_HARD_WIND_SAMPLES; s++ {
		windOffset := rand.NormFloat64() * AI_HARD_WIND_SIGMA
		ix, iy := simShotWithWind(gs, slot, x0, y0, dir, ang, pwr, gs.Wind+windOffset)
		best := -math.MaxFloat64
		for _, t := range targets {
			if shrapnel && t.kind != "cannon" {
				continue
			}
			d := math.Hypot(ix-t.x, iy-t.y)
			var sc float64
			if shrapnel {
				// Within fragment spread radius → effectively a hit.
				radius := float64(SHRAPNEL_BLAST_R + 16)
				if d <= radius {
					sc = -0.0 + t.weight*40
				} else {
					sc = -(d - radius) + t.weight*40
				}
			} else {
				sc = -d + t.weight*40
			}
			if sc > best {
				best = sc
			}
		}
		if best == -math.MaxFloat64 {
			best = -1e6 // no eligible target on this sample
		}
		sum += best
	}
	return sum / float64(AI_HARD_WIND_SAMPLES)
}

// collectTargets builds the priority list with pressure-adjusted weights.
//   pressure > +0.25 (ahead) → push for kill: throne and cannons get more weight.
//   pressure < -0.25 (behind) → defend: prioritise removing threats and income.
//   opponent has ≥2 alive cannons → cannon weight floor of 1.6 either way.
func collectTargets(opp *Castle, pressure float64) []aiTarget {
	cannonW := 1.0
	throneW := 1.5
	towerW := 0.5
	vaneW := 0.4
	keepW := 0.2

	if countAliveCannons(opp) >= 2 && cannonW < 1.6 {
		cannonW = 1.6
	}
	if pressure > AI_HARD_PRESSURE_AGGR_THRESHOLD {
		throneW += 0.5
		cannonW += 0.2
	} else if pressure < AI_HARD_PRESSURE_DEF_THRESHOLD {
		cannonW += 0.4
		towerW += 0.2
		throneW -= 0.3
	}

	var ts []aiTarget
	for _, cn := range opp.Cannons {
		if cn.Alive {
			ts = append(ts, aiTarget{x: float64(cn.BX), y: float64(cn.BY), weight: cannonW, kind: "cannon"})
		}
	}
	if opp.King.Alive {
		ts = append(ts, aiTarget{x: float64(opp.King.X), y: float64(opp.King.Y), weight: throneW, kind: "throne"})
	}
	for _, t := range opp.Towers {
		if t.HP > 0 {
			ts = append(ts, aiTarget{
				x:      float64(t.X + t.W/2),
				y:      float64(t.Y + t.H/2),
				weight: towerW,
				kind:   "tower",
			})
		}
	}
	if opp.WindVane.Alive {
		ts = append(ts, aiTarget{
			x:      float64(opp.WindVane.X),
			y:      float64(opp.WindVane.Y + (opp.WindVane.TowerY-opp.WindVane.Y)/2),
			weight: vaneW,
			kind:   "vane",
		})
	}
	ts = append(ts, aiTarget{
		x:      float64(opp.BaseX + BASE_W/2),
		y:      float64(opp.GroundPx) - 100,
		weight: keepW,
		kind:   "keep",
	})
	return ts
}

// simShot replays the engine's projectile physics with the current gs.Wind.
// Read-only — never mutates gs.
func simShot(gs *GS, slot int, x0, y0, dir, angleDeg, power float64) (float64, float64) {
	return simShotWithWind(gs, slot, x0, y0, dir, angleDeg, power, gs.Wind)
}

// simShotWithWind is simShot with an explicit wind value, used by the Hard AI's
// Monte Carlo over wind gusts.
func simShotWithWind(gs *GS, slot int, x0, y0, dir, angleDeg, power, wind float64) (float64, float64) {
	rad := angleDeg * math.Pi / 180
	v := power * gs.PTOS
	x := x0
	y := y0
	vx := math.Cos(rad) * v * dir
	vy := -math.Sin(rad) * v

	// Cap iteration: a 50-power 45° shot lasts a few hundred micro-steps; 6000 is generous.
	for step := 0; step < 6000; step++ {
		vy += GRAVITY
		vx += wind * WIND_C
		x += vx
		y += vy

		if x < 0 || x >= float64(gs.W) {
			return x, y
		}
		if y >= H {
			return x, float64(H)
		}

		ix := int(x)
		iy := int(y)

		// Terrain hit
		if ix >= 0 && ix < gs.W {
			if uint16Idx := gs.Terrain.Data[ix]; iy >= H-int(uint16Idx) {
				return x, float64(H - int(uint16Idx))
			}
		}

		// Wall / cannon / tower collisions on either castle.
		// Cannons (block first since they sit on towers and we want to count them as hits).
		for ci := 0; ci < 2; ci++ {
			c := &gs.Castles[ci]
			for _, cn := range c.Cannons {
				if cn.Alive && absInt(ix-cn.BX) <= 16 && absInt(iy-cn.BY) <= 14 {
					return x, y
				}
			}
			// Walls (only count alive bricks so freshly-blown gaps aren't blockers).
			for wi := range c.Walls {
				if hitW(c.Walls[wi], ix, iy) {
					return x, y
				}
			}
			// Towers
			for _, t := range c.Towers {
				if t.HP > 0 && ix >= t.X && ix < t.X+t.W && iy >= t.Y && iy < t.Y+t.H {
					return x, y
				}
			}
			// Chambers
			for _, ch := range c.Chambers {
				if ch.Alive && ix >= ch.X && ix < ch.X+ch.W && iy >= ch.Y && iy < ch.Y+ch.H {
					return x, y
				}
			}
		}
	}
	return x, y
}
