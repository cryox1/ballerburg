// Copyright 2026 Jonas Bartel
// SPDX-License-Identifier: Apache-2.0

package main

// Ballerburg engine — pure game logic.
// Mirrors the single-player original at snippets/ballerburg-fix/index.html.
// All JSON field names are lowercase to match the client's reads verbatim.

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"time"
)

// ─── Constants ──────────────────────────────────────────────────────────────
const (
	H      = 1800
	BASE_W = 580
	FLAT_W = 860

	// Per-game random map width band. The actual W for a match is drawn
	// from [W_MIN, W_MAX] at NewEng() time and stored on GS.W; PTOS is
	// rebalanced per-match so the minimum power to cross at 45° lands in
	// the 20..40 band regardless of map size.
	W_MIN = 2720
	W_MAX = 3680

	GRAVITY  = 0.14
	SUBSTEPS = 6
	WIND_C         = 0.012
	WIND_GUST_DELTA = 0.05 // per-substep random walk step for mid-flight gust
	WIND_GUST_MAX   = 0.6  // max gust magnitude (clamp)

	EXP_R  = 120 // normal-shot blast crater (terrain) — bumped from 80 (+50%) for chunkier normal hits
	EXP_RP = 120 // powder-chamber detonation crater (left alone)

	THRONE_BLAST_MARGIN = 30 // throne is destroyed only by explosions within (rad - this) of its center — smaller hitbox than other chambers
	WCR    = 60  // normal-shot brick-kill radius — bumped from 40 (+50%); kept below WCRP so powder-chamber stays the bigger wall-stripper
	WCRP   = 72  // powder-chamber brick-kill radius (left alone)

	// Schrapnell — 4 small fragments, low impact, can still kill cannons/vane.
	// Blast radius & brick-kill scaled by the same +50% as the normal cannon.
	SHRAPNEL_COUNT     = 4
	SHRAPNEL_BLAST_R   = 38 // was 25; +50% to track normal-cannon scaling
	SHRAPNEL_BLAST_WCR = 27 // was 18; +50% to track normal-cannon scaling
	SHRAPNEL_GUST      = 0.15 // 3× normal random walk → wide drift
	SHRAPNEL_GUST_MAX  = 1.2  // wider clamp than normal so the drift is real

	// Dreitürme layout (mirrors original_index/index.html lines 538–597).
	// Wall dimensions are snapped to multiples of BS=10 so the brick grid
	// fills each wall exactly — otherwise integer division in addWall leaves
	// 1–9px shortfalls that show as gaps between abutting walls (vane-tower
	// to keep, flanking tower to curtain, etc.).
	FLOOR_H   = 80  // ground-floor chamber zone height
	LT_W      = 110 // flanking tower width
	LT_H      = 270 // flanking tower height
	CK_W      = 200 // central keep width
	CK_H      = 220 // central keep height
	CW_H      = 50  // curtain wall height
	CW_Y_OFF  = 130 // curtain wall offset above ground
	VANE_TW_W = 40  // vane-tower width
	VANE_TW_H = 110 // vane-tower height
	BS        = 10  // brick edge length

	INIT_GOLD    = 500
	INIT_POWDER  = 280
	INIT_AMMO    = 15
	INIT_POP     = 120
	INIT_CANNONS = 2

	POP_MAX          = 400
	POP_MIN          = 0
	MAX_TOWERS       = 2
	BASE_TAX_RATE    = 2.3  // gold per pop per 100% tax (before round scale)
	TAX_ROUND_SCALE  = 0.09 // applied to tax income each turn
	TOWER_GOLD_BONUS = 24   // per alive Förderturm — balanced (was 33, before 18)
	TOWER_UPKEEP     = 8    // per alive Förderturm per round
	MAX_ROUND        = 40

	PRICE_POWDER      = 1
	PRICE_AMMO        = 4
	PRICE_TOWER       = 220
	TOWER_SELL_REFUND = PRICE_TOWER / 2
	PRICE_VANE        = 120
	PRICE_CANNON      = 260
	PRICE_BRICK_PACK  = 90
	BRICK_PACK_AMOUNT = 20
	PRICE_SHRAPNEL    = 100 // 1 Schrapnell shot (= 4 fragments)
)

// ─── Types ──────────────────────────────────────────────────────────────────

type TSnap struct {
	Data []uint16 `json:"data"`
}

type Chamber struct {
	Kind  string `json:"kind"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	Alive bool   `json:"alive"`
}

type Cannon struct {
	BX    int  `json:"bx"`
	BY    int  `json:"by"`
	Side  int  `json:"side"`
	Alive bool `json:"alive"`
}

// Wall.Bricks is []int8 so JSON encodes it as an array of 0/1 integers
// (Go marshals []byte/[]uint8 as base64; the client expects an array).
type Wall struct {
	Kind   string `json:"kind"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	Cols   int    `json:"cols"`
	Rows   int    `json:"rows"`
	CellW  int    `json:"cellw"`
	CellH  int    `json:"cellh"`
	Bricks []int8 `json:"bricks"`
	HP     int    `json:"hp"`
	HPMax  int    `json:"hpmax"`
}

type KState struct {
	X     int  `json:"x"`
	Y     int  `json:"y"`
	Alive bool `json:"alive"`
}

type VState struct {
	X      int  `json:"x"`
	Y      int  `json:"y"`
	TowerY int  `json:"towery"`
	Alive  bool `json:"alive"`
	Owned  bool `json:"owned"`
}

type Twr struct {
	X     int `json:"x"`
	Y     int `json:"y"`
	W     int `json:"w"`
	H     int `json:"h"`
	HP    int `json:"hp"`
	HPMax int `json:"hpmax"`
}

type Castle struct {
	Side          int       `json:"side"`
	BaseX         int       `json:"basex"`
	BaseW         int       `json:"basew"`
	GroundPx      int       `json:"groundpx"`
	BuildTopY     int       `json:"buildtopy"`
	Gold          int       `json:"gold"`
	Powder        int       `json:"powder"`
	Ammo          int       `json:"ammo"`
	ShrapnelAmmo  int       `json:"shrapnelammo"`

	Population    int       `json:"population"`
	TaxRate       int       `json:"taxrate"`
	PendingBricks int       `json:"pendingbricks"`
	MaxCannons    int       `json:"maxcannons"`
	Cannons       []Cannon  `json:"cannons"`
	Walls         []Wall    `json:"walls"`
	Chambers      []Chamber `json:"chambers"`
	King          KState    `json:"king"`
	WindVane      VState    `json:"windvane"`
	Towers        []Twr     `json:"towers"`
}

type Pt struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Pro struct {
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	VX        float64 `json:"vx"`
	VY        float64 `json:"vy"`
	PType     string  `json:"ptype"` // "" (normal) | "shrapnel"
	Trail     []Pt    `json:"trail"`
	OwnerSide int     `json:"-"`
	OwnerIdx  int     `json:"-"`
	WindGust  float64 `json:"-"` // mid-flight gust fluctuation, not broadcast
}

type GS struct {
	ID          string    `json:"id"`
	Round       int       `json:"round"`
	Phase       string    `json:"phase"` // "aim" | "flying" | "gameover"
	Turn        int       `json:"turn"`
	Wind        float64   `json:"wind"`
	WindExact   bool      `json:"windexact"`
	W           int       `json:"w"`    // per-game map width in px (was const)
	PTOS        float64   `json:"ptos"` // per-game velocity-per-powder factor
	Terrain     TSnap     `json:"terrain"`
	Castles     [2]Castle `json:"castles"`
	Projectiles []*Pro    `json:"projectiles"`
	AIAction    string    `json:"aiaction"` // e.g. "Computer baut Mauern" while AI works between commands
	Players     [2]string `json:"players"`
	Teams       [2]string `json:"teams"`
	Winner      *int      `json:"winner,omitempty"`
	Reason      string    `json:"reason,omitempty"`
}

type Engine struct {
	GS GS
}

// Command payloads.
type FireP struct {
	Cannon   int    `json:"cannon"`
	Angle    int    `json:"angle"`
	Power    int    `json:"power"`
	AmmoType string `json:"ammoType"` // "" (normal) | "shrapnel"
}
type BuyP struct {
	Item string `json:"item"`
}
type TaxP struct {
	Tax int `json:"tax"`
}
type PlaceBrickP struct {
	X int `json:"x"`
	Y int `json:"y"`
}
type SellTowerP struct {
	Index int `json:"index"`
}

type Cmd struct {
	Action  string `json:"action"`
	Payload any    `json:"payload"`
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func genCode() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)[:6]
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ─── Terrain ────────────────────────────────────────────────────────────────

// genTerrain builds the terrain for a map of width mapW. The central hill is
// capped so a 45° shot at the matching P_target power from cannon-top height
// clears it with margin (vTarget² = D*GRAVITY by construction in NewEng).
func genTerrain(mapW int) TSnap {
	seg := make([]float64, mapW)
	seg[0] = 350 + rand.Float64()*120
	seg[mapW-1] = 350 + rand.Float64()*120
	var disp func(l, r int, rf float64)
	disp = func(l, r int, rf float64) {
		if r-l <= 1 {
			return
		}
		m := (l + r) >> 1
		v := (seg[l]+seg[r])/2 + (rand.Float64()-0.5)*float64(r-l)*rf
		seg[m] = math.Max(210, math.Min(float64(H)-290, v))
		disp(l, m, rf*0.6)
		disp(m, r, rf*0.6)
	}
	disp(0, mapW-1, 1.4)

	midX := mapW / 2
	// Lowered random peak band (was 440..660). Combined with the per-x
	// clearance cap below, this keeps the central hill from blocking a
	// 45° / P_target shot from cannon-top height.
	peak := 250.0 + rand.Float64()*180.0
	mH := 640

	// Clearance budget: at 45° with velocity v=Ptarget*ptos the trajectory
	// apex sits v²/(4g) above the muzzle. The cannon muzzle is at roughly
	// flat-zone baseline minus LT_H (flanking-tower height). We cap the
	// hill so its top stays at least `margin` px below the apex.
	flatBaseline := seg[FLAT_W/2]
	launchY := float64(H) - flatBaseline - float64(LT_H)
	D := float64(mapW - BASE_W)
	vTarget := math.Sqrt(D * GRAVITY) // == Ptarget*ptos by construction in NewEng
	apexAboveLaunch := vTarget * vTarget / (4 * GRAVITY)
	const clearanceMargin = 80.0
	maxHillTopY := launchY - apexAboveLaunch + clearanceMargin
	// seg holds height-from-bottom; canvas y = H - seg[x]. We want
	// H - seg[x] >= maxHillTopY, i.e. seg[x] <= H - maxHillTopY.
	hillSegCap := float64(H) - maxHillTopY

	for x := 0; x < mapW; x++ {
		d := math.Abs(float64(x - midX))
		a := math.Min(math.Pi/2, (d/float64(mH))*(math.Pi/2))
		newH := seg[x] + peak*math.Cos(a)
		if newH > hillSegCap {
			newH = hillSegCap
		}
		seg[x] = math.Min(float64(H)-80, newH)
	}

	f1 := seg[FLAT_W/2]
	f2 := seg[mapW-1-FLAT_W/2]
	for x := 0; x < FLAT_W; x++ {
		seg[x] = f1
	}
	for x := mapW - FLAT_W; x < mapW; x++ {
		seg[x] = f2
	}
	for i := 0; i < 160; i++ {
		t := float64(i) / 160
		seg[FLAT_W+i] = f1*(1-t) + seg[FLAT_W+160]*t
		seg[mapW-1-FLAT_W-i] = f2*(1-t) + seg[mapW-1-FLAT_W-160]*t
	}

	d := make([]uint16, mapW)
	for x := 0; x < mapW; x++ {
		d[x] = uint16(math.Round(seg[x]))
	}
	return TSnap{Data: d}
}

// ─── Castle construction ────────────────────────────────────────────────────

// mkCastle builds the Dreitürme layout — flanking L/R towers (powder/ammo
// chambers + cannons), central keep (throne + treasure), partition wall, and
// vane-tower atop the keep. Chambers sit at ground-floor level and are only
// reachable once their enclosing wall is breached. Mirrors original_index/
// index.html:508–649.
func mkCastle(side, mapW int, trk []uint16) Castle {
	bX := 0
	if side == 1 {
		bX = mapW - BASE_W
	}
	cX := bX + BASE_W/2
	gY := int(trk[cX])
	gPx := H - gY

	var walls []Wall
	addWall := func(kind string, ax, ay, aw, ah int) {
		cs := int(math.Max(1, math.Round(float64(aw)/BS)))
		rs := int(math.Max(1, math.Round(float64(ah)/BS)))
		cw := aw / cs
		ch := ah / rs
		if cw <= 0 {
			cw = 1
		}
		if ch <= 0 {
			ch = 1
		}
		n := cs * rs
		br := make([]int8, n)
		for i := range br {
			br[i] = 1
		}
		walls = append(walls, Wall{
			Kind: kind, X: ax, Y: ay, W: aw, H: ah,
			Cols: cs, Rows: rs,
			CellW: cw, CellH: ch,
			Bricks: br, HP: n, HPMax: n,
		})
	}

	ltX := bX + 10
	rtX := bX + BASE_W - 10 - LT_W
	flankY := gPx - LT_H // both flanking towers share the same top
	ckX := bX + (BASE_W-CK_W)/2
	ckY := gPx - CK_H
	cwY := gPx - CW_Y_OFF
	floorY := gPx - FLOOR_H

	// Flanking towers + battlements
	addWall("tower-out", ltX, flankY, LT_W, LT_H)
	addWall("tower-out", rtX, flankY, LT_W, LT_H)
	for i := 0; i < 4; i++ {
		addWall("battlement", ltX+6+i*24, flankY-20, 14, 20)
		addWall("battlement", rtX+6+i*24, flankY-20, 14, 20)
	}

	// Central keep + battlements. Skip battlements whose footprint sits under
	// the vane-tower body — the vane stands on the keep roof and would
	// otherwise overlap them.
	addWall("keep-wall", ckX, ckY, CK_W, CK_H)
	vtL := cX - VANE_TW_W/2
	vtR := vtL + VANE_TW_W
	for i := 0; i < 6; i++ {
		bx := ckX + 6 + i*30
		if bx+18 > vtL && bx < vtR {
			continue
		}
		addWall("battlement", bx, ckY-24, 18, 24)
	}
	// Symmetric layout: from each player's own perspective the front (toward
	// opponent) flanking tower holds AMMO + the primary cannon, and the rear
	// flanking tower holds POWDER + the secondary cannon. The keep splits the
	// same way: TREASURE in the front half, THRONE (with king) in the rear
	// half. side=0 faces right (rtX is front); side=1 faces left (ltX is front)
	// — so we mirror side=1 horizontally. Diverges from the single-player
	// layout, which was a translated copy and therefore asymmetric.
	frontTowerX := rtX
	rearTowerX := ltX
	frontHalfX := ckX + CK_W/2 + 4 // right half of keep
	rearHalfX := ckX + 8           // left half of keep
	throneRingX := ckX + 12        // covers left half (= rear) of keep
	if side == 1 {
		frontTowerX = ltX
		rearTowerX = rtX
		frontHalfX = ckX + 8
		rearHalfX = ckX + CK_W/2 + 4
		throneRingX = ckX + CK_W/2 // covers right half (= rear) of keep
	}

	// Throne-wall: extra-thin ring above the throne chamber inside the keep.
	twT := 12
	addWall("throne-wall", throneRingX, floorY-twT, CK_W/2-twT, twT)

	// Curtain walls: flanking tower → central keep, both at mid-height and floor level
	lcwX := ltX + LT_W
	lcwW := ckX - lcwX
	rcwX := ckX + CK_W
	rcwW := rtX - rcwX
	if lcwW > 0 {
		addWall("keep-wall", lcwX, cwY, lcwW, CW_H)
		addWall("keep-wall", lcwX, floorY, lcwW, gPx-floorY)
	}
	if rcwW > 0 {
		addWall("keep-wall", rcwX, cwY, rcwW, CW_H)
		addWall("keep-wall", rcwX, floorY, rcwW, gPx-floorY)
	}

	// Partition wall between throne and treasure inside the central keep
	partX := ckX + CK_W/2 - 5
	addWall("partition", partX, floorY-20, 10, FLOOR_H+20)

	// Vane-tower (hosts wind vane) atop central keep
	vtX := cX - VANE_TW_W/2
	vtY := ckY - VANE_TW_H
	addWall("vane-tower", vtX, vtY, VANE_TW_W, VANE_TW_H)
	addWall("battlement", vtX+4, vtY-24, 36, 24)
	vane := VState{X: cX, Y: vtY - 48, TowerY: vtY, Alive: true, Owned: true}

	// Chambers at ground-floor level (laid out per the front/rear assignment
	// above so both castles look identical from each owner's perspective).
	halfCK := (CK_W - 22) / 2
	chambers := []Chamber{
		{Kind: "throne", X: rearHalfX, Y: floorY, W: halfCK, H: FLOOR_H, Alive: true},
		{Kind: "treasure", X: frontHalfX, Y: floorY, W: halfCK, H: FLOOR_H, Alive: true},
		{Kind: "powder", X: rearTowerX + 8, Y: floorY, W: LT_W - 16, H: FLOOR_H, Alive: true},
		{Kind: "ammo", X: frontTowerX + 8, Y: floorY, W: LT_W - 16, H: FLOOR_H, Alive: true},
	}

	// King inside throne chamber (chambers[0] is throne by construction)
	thr := chambers[0]
	king := KState{X: thr.X + thr.W/2, Y: thr.Y + thr.H - 26, Alive: true}

	// Cannons: cannons[0] = front (in ammo tower), cannons[1] = rear (in powder
	// tower). Same numbering for both sides so "Kanone 1" always denotes the
	// forward gun.
	cnSide := 1
	if side == 1 {
		cnSide = -1
	}
	cannons := []Cannon{
		{BX: frontTowerX + LT_W/2, BY: gPx - LT_H - 28, Side: cnSide, Alive: true},
		{BX: rearTowerX + LT_W/2, BY: gPx - LT_H - 28, Side: cnSide, Alive: true},
	}

	// BuildTopY = topmost wall y (for brick-placement vertical clamp)
	buildTopY := gPx
	for _, w := range walls {
		if w.Y < buildTopY {
			buildTopY = w.Y
		}
	}

	return Castle{
		Side: side, BaseX: bX, BaseW: BASE_W, GroundPx: gPx, BuildTopY: buildTopY,
		Gold: INIT_GOLD, Powder: INIT_POWDER, Ammo: INIT_AMMO,
		Population: INIT_POP, TaxRate: 40, MaxCannons: INIT_CANNONS,
		Cannons: cannons, Walls: walls, Chambers: chambers,
		King: king, WindVane: vane, Towers: []Twr{},
	}
}

// ─── Wall brick helpers ─────────────────────────────────────────────────────

func carveWB(w *Wall, cx, cy, rad int) int {
	if w.HP <= 0 {
		return 0
	}
	r2 := float64(rad) * float64(rad)
	k := 0
	for ry := 0; ry < w.Rows; ry++ {
		for rx := 0; rx < w.Cols; rx++ {
			if w.Bricks[ry*w.Cols+rx] == 0 {
				continue
			}
			bx := float64(w.X) + (float64(rx)+0.5)*float64(w.CellW)
			by := float64(w.Y) + (float64(ry)+0.5)*float64(w.CellH)
			dx := bx - float64(cx)
			dy := by - float64(cy)
			if dx*dx+dy*dy <= r2 {
				w.Bricks[ry*w.Cols+rx] = 0
				k++
			}
		}
	}
	w.HP = max(0, w.HP-k)
	return k
}

// cannonSupported returns true while at least one alive brick remains in the
// short column directly beneath the cannon's breech. Cannons rest 28px above
// their flanking-tower roof; once that roof has been carved away the gun is
// floating and should fall.
func cannonSupported(cs *Castle, bx, by int) bool {
	for sy := by + 14; sy <= by+58; sy += BS {
		for sx := bx - 18; sx <= bx+18; sx += BS {
			for wi := range cs.Walls {
				if hitW(cs.Walls[wi], sx, sy) {
					return true
				}
			}
		}
	}
	return false
}

func hitW(w Wall, sx, sy int) bool {
	if w.HP <= 0 {
		return false
	}
	if sx < w.X || sx >= w.X+w.W || sy < w.Y || sy >= w.Y+w.H {
		return false
	}
	rx := clampInt((sx-w.X)/w.CellW, 0, w.Cols-1)
	ry := clampInt((sy-w.Y)/w.CellH, 0, w.Rows-1)
	return w.Bricks[ry*w.Cols+rx] == 1
}

// addSupportRow lays one BS-tall row of bricks across [xStart, xEnd) at row
// top y. Each cell either revives a slot in an existing wall (bumping HP up
// to HPMax) or appends a fresh 1×1 keep-wall, mirroring PlaceBrick. Used when
// repairing a cannon/windvane so the structure has visible support and
// cannonSupported() finds at least one alive brick beneath it.
func addSupportRow(cs *Castle, xStart, xEnd, y int) {
	by := (y / BS) * BS
	for x := (xStart / BS) * BS; x < xEnd; x += BS {
		var found *Wall
		for i := range cs.Walls {
			w := &cs.Walls[i]
			if x >= w.X && x < w.X+w.W && by >= w.Y && by < w.Y+w.H {
				found = w
				break
			}
		}
		if found != nil {
			rx := clampInt((x-found.X)/found.CellW, 0, found.Cols-1)
			ry := clampInt((by-found.Y)/found.CellH, 0, found.Rows-1)
			idx := ry*found.Cols + rx
			if found.Bricks[idx] == 0 {
				found.Bricks[idx] = 1
				found.HP++
				if found.HP > found.HPMax {
					found.HP = found.HPMax
				}
			}
		} else {
			// Same anti-stacking guard as PlaceBrick: don't drop a fresh 1×1
			// wall on top of a non-BS-aligned battlement/partition.
			if cellOverlapsAliveBrick(cs.Walls, x, by, BS, BS) {
				continue
			}
			cs.Walls = append(cs.Walls, Wall{
				Kind: "keep-wall", X: x, Y: by, W: BS, H: BS,
				Cols: 1, Rows: 1, CellW: BS, CellH: BS,
				Bricks: []int8{1}, HP: 1, HPMax: 1,
			})
		}
	}
}

// ─── Engine constructor ─────────────────────────────────────────────────────

func NewEng() *Engine {
	rand.Seed(time.Now().UnixNano())
	// Random map width per match. Castle-to-castle distance varies, which
	// forces players to re-test power/angle each game instead of reusing
	// a memorized recipe.
	mapW := W_MIN + rand.Intn(W_MAX-W_MIN+1)
	// Velocity-per-powder is rebalanced so the minimum power to land a
	// 45° shot at the opposing castle interpolates from 20 (smallest map)
	// to 40 (largest map). Per-shot powder cost stays 1:1 with the slider
	// value, so the "below 40 powder to cross" invariant holds.
	D := float64(mapW - BASE_W)
	t := float64(mapW-W_MIN) / float64(W_MAX-W_MIN)
	pTarget := 20.0 + t*20.0
	ptos := math.Sqrt(D*GRAVITY) / pTarget
	trk := genTerrain(mapW)
	return &Engine{GS: GS{
		ID: genCode(), Round: 1, Phase: "aim", Turn: 0,
		Wind:      rand.Float64()*3.15 - 1.575,
		WindExact: true,
		W:         mapW,
		PTOS:      ptos,
		Terrain:   trk,
		Castles:   [2]Castle{mkCastle(0, mapW, trk.Data), mkCastle(1, mapW, trk.Data)},
		Players:   [2]string{"", ""},
	}}
}

// ─── Castle scoring (for round-limit tiebreak) ──────────────────────────────

func cVal(c Castle) int {
	v := 0
	for _, cn := range c.Cannons {
		if cn.Alive {
			v += 100
		}
	}
	for _, t := range c.Towers {
		if t.HP > 0 {
			v += 120
		}
	}
	if c.WindVane.Alive && c.WindVane.Owned {
		v += 80
	}
	return v + c.Gold + c.Powder*2 + c.Ammo*5 + c.Population
}

// ─── Commands ───────────────────────────────────────────────────────────────

func (e *Engine) Fire(pIdx, cannon, angle, power int, ammoType string) error {
	if e.GS.Phase != "aim" {
		return fmt.Errorf("not in aim phase")
	}
	if pIdx != e.GS.Turn {
		return fmt.Errorf("not your turn")
	}
	if angle < 10 || angle > 90 {
		return fmt.Errorf("angle out of range")
	}
	if power < 5 || power > 50 {
		return fmt.Errorf("power out of range")
	}
	cs := &e.GS.Castles[pIdx]
	if cs.PendingBricks > 0 {
		return fmt.Errorf("place all bricks before firing")
	}
	if cannon < 0 || cannon >= len(cs.Cannons) || !cs.Cannons[cannon].Alive {
		return fmt.Errorf("invalid cannon")
	}
	switch ammoType {
	case "", "normal":
		if cs.Ammo <= 0 {
			return fmt.Errorf("no ammo")
		}
	case "shrapnel":
		if cs.ShrapnelAmmo <= 0 {
			return fmt.Errorf("kein Schrapnell")
		}
	default:
		return fmt.Errorf("unknown ammo type: %s", ammoType)
	}
	if cs.Powder < power {
		return fmt.Errorf("insufficient powder")
	}
	cs.Powder -= power

	cn := cs.Cannons[cannon]
	spd := float64(power) * e.GS.PTOS
	rad := float64(angle) * math.Pi / 180
	dir := float64(cn.Side)
	bx := float64(cn.BX) + dir*18
	by := float64(cn.BY) - 8

	e.GS.Phase = "flying"

	switch ammoType {
	case "shrapnel":
		cs.ShrapnelAmmo--
		for i := 0; i < SHRAPNEL_COUNT; i++ {
			// ±3° angle, ±10% power per fragment
			dAng := (rand.Float64()*6 - 3) * math.Pi / 180
			dPwr := 0.9 + rand.Float64()*0.2
			subSpd := spd * dPwr
			subRad := rad + dAng
			e.GS.Projectiles = append(e.GS.Projectiles, &Pro{
				X:         bx,
				Y:         by,
				VX:        math.Cos(subRad) * subSpd * dir,
				VY:        -math.Sin(subRad) * subSpd,
				PType:     "shrapnel",
				Trail:     []Pt{},
				OwnerSide: pIdx,
				OwnerIdx:  cannon,
			})
		}
	default: // normal
		cs.Ammo--
		e.GS.Projectiles = append(e.GS.Projectiles, &Pro{
			X:         bx,
			Y:         by,
			VX:        math.Cos(rad) * spd * dir,
			VY:        -math.Sin(rad) * spd,
			Trail:     []Pt{},
			OwnerSide: pIdx,
			OwnerIdx:  cannon,
		})
	}
	return nil
}

// reviveChamber sets a destroyed chamber (by kind) back to alive. Used when
// the player buys a fresh stockpile to refill a previously-blown room.
func reviveChamber(cs *Castle, kind string) {
	for i := range cs.Chambers {
		if cs.Chambers[i].Kind == kind && !cs.Chambers[i].Alive {
			cs.Chambers[i].Alive = true
			return
		}
	}
}

func towerForSlot(cs *Castle, pIdx, slot, mapW int, terrain []uint16) Twr {
	dir := 1
	if pIdx == 1 {
		dir = -1
	}
	sX := float64(cs.BaseX + cs.BaseW + 40)
	if pIdx == 1 {
		sX = float64(cs.BaseX - 40)
	}
	tx := sX + float64(slot)*80*float64(dir)
	txi := clampInt(int(tx), 0, mapW-1)
	gPx := H - int(terrain[txi])
	return Twr{X: int(tx - 22), Y: gPx - 160, W: 44, H: 160, HP: 2, HPMax: 2}
}

func relayoutTowers(cs *Castle, pIdx, mapW int, terrain []uint16) {
	for i := range cs.Towers {
		oldHP := cs.Towers[i].HP
		oldHPMax := cs.Towers[i].HPMax
		if oldHPMax <= 0 {
			oldHPMax = 2
		}
		t := towerForSlot(cs, pIdx, i, mapW, terrain)
		t.HPMax = oldHPMax
		t.HP = clampInt(oldHP, 0, oldHPMax)
		cs.Towers[i] = t
	}
}

func (e *Engine) Buy(pIdx int, item string) error {
	if pIdx != e.GS.Turn {
		return fmt.Errorf("not your turn")
	}
	cs := &e.GS.Castles[pIdx]
	switch item {
	case "buy-powder-50":
		if cs.Gold < PRICE_POWDER*50 {
			return fmt.Errorf("insufficient gold")
		}
		cs.Gold -= PRICE_POWDER * 50
		cs.Powder += 50
		reviveChamber(cs, "powder")
	case "buy-ammo-5":
		if cs.Gold < PRICE_AMMO*5 {
			return fmt.Errorf("insufficient gold")
		}
		cs.Gold -= PRICE_AMMO * 5
		cs.Ammo += 5
		reviveChamber(cs, "ammo")
	case "buy-shrapnel":
		if cs.Gold < PRICE_SHRAPNEL {
			return fmt.Errorf("insufficient gold")
		}
		cs.Gold -= PRICE_SHRAPNEL
		cs.ShrapnelAmmo++
	case "buy-bricks":
		if cs.Gold < PRICE_BRICK_PACK {
			return fmt.Errorf("insufficient gold")
		}
		cs.Gold -= PRICE_BRICK_PACK
		cs.PendingBricks += BRICK_PACK_AMOUNT
	case "buy-tower":
		if cs.Gold < PRICE_TOWER || len(cs.Towers) >= MAX_TOWERS {
			return fmt.Errorf("cannot buy tower")
		}
		cs.Gold -= PRICE_TOWER
		cs.Towers = append(cs.Towers, towerForSlot(cs, pIdx, len(cs.Towers), e.GS.W, e.GS.Terrain.Data))
	case "buy-windvane":
		// Allow purchase whenever the vane is currently absent — first-time
		// purchase OR rebuild after destruction. Original_index/index.html
		// treats the vane as a one-time item with a rebuild option.
		if cs.Gold < PRICE_VANE || cs.WindVane.Alive {
			return fmt.Errorf("cannot buy windvane")
		}
		cs.Gold -= PRICE_VANE
		cs.WindVane.Alive = true
		cs.WindVane.Owned = true
		// Rebuild the full vane-tower wall so the flag stands on a solid tower.
		// addSupportRow (used for cannons) only restores the top row, which
		// left the flag floating on a sliver and let the floating-vane cascade
		// in applyExplosion remove it on the next explosion.
		for i := range cs.Walls {
			w := &cs.Walls[i]
			if w.Kind == "vane-tower" {
				for j := range w.Bricks {
					w.Bricks[j] = 1
				}
				w.HP = w.HPMax
			}
		}
	case "buy-cannon":
		al := 0
		for _, c := range cs.Cannons {
			if c.Alive {
				al++
			}
		}
		if cs.Gold < PRICE_CANNON || al >= cs.MaxCannons {
			return fmt.Errorf("cannot buy cannon")
		}
		cs.Gold -= PRICE_CANNON
		for i := range cs.Cannons {
			if !cs.Cannons[i].Alive {
				cs.Cannons[i].Alive = true
				cn := &cs.Cannons[i]
				// Restore one row of bricks at the original flanking-tower
				// roof line (cn.BY+28 = flankY) so the cannon has visible
				// support and cannonSupported() finds at least one brick.
				addSupportRow(cs, cn.BX-LT_W/2, cn.BX+LT_W/2, cn.BY+28)
				break
			}
		}
	default:
		return fmt.Errorf("unknown item: %s", item)
	}
	return nil
}

func (e *Engine) SellTower(pIdx, idx int) error {
	if pIdx != e.GS.Turn {
		return fmt.Errorf("not your turn")
	}
	cs := &e.GS.Castles[pIdx]
	if idx < 0 || idx >= len(cs.Towers) {
		return fmt.Errorf("invalid tower")
	}
	if cs.Towers[idx].HP <= 0 {
		return fmt.Errorf("tower destroyed")
	}
	cs.Towers = append(cs.Towers[:idx], cs.Towers[idx+1:]...)
	cs.Gold += TOWER_SELL_REFUND
	relayoutTowers(cs, pIdx, e.GS.W, e.GS.Terrain.Data)
	return nil
}

// PlaceBrick consumes one PendingBrick to either revive a dead cell in an
// existing wall or add a fresh 1×1 wall. Mirrors original_index/index.html
// canvas-click handler at lines 897–977.
func (e *Engine) PlaceBrick(pIdx, x, y int) error {
	if e.GS.Phase != "aim" {
		return fmt.Errorf("not in aim phase")
	}
	if pIdx != e.GS.Turn {
		return fmt.Errorf("not your turn")
	}
	cs := &e.GS.Castles[pIdx]
	if cs.PendingBricks <= 0 {
		return fmt.Errorf("no bricks to place")
	}
	leftBound := cs.BaseX + 4
	rightBound := cs.BaseX + cs.BaseW - 4
	upperLimit := cs.BuildTopY - BS*10
	if upperLimit < 0 {
		upperLimit = 0
	}
	if x < leftBound || x > rightBound || y < upperLimit || y > cs.GroundPx {
		return fmt.Errorf("out of build area")
	}

	// Find a wall already containing (x,y), if any.
	var clicked *Wall
	for i := range cs.Walls {
		w := &cs.Walls[i]
		if x >= w.X && x < w.X+w.W && y >= w.Y && y < w.Y+w.H {
			clicked = w
			break
		}
	}

	hasNeighbor := func(cx, cy float64) bool {
		offs := [4][2]int{{-BS, 0}, {BS, 0}, {0, -BS}, {0, BS}}
		for _, o := range offs {
			nx := int(cx) + o[0]
			ny := int(cy) + o[1]
			for i := range cs.Walls {
				if hitW(cs.Walls[i], nx, ny) {
					return true
				}
			}
		}
		return false
	}

	if clicked != nil {
		rx := clampInt((x-clicked.X)/clicked.CellW, 0, clicked.Cols-1)
		ry := clampInt((y-clicked.Y)/clicked.CellH, 0, clicked.Rows-1)
		idx := ry*clicked.Cols + rx
		if clicked.Bricks[idx] == 1 {
			return fmt.Errorf("brick already there")
		}
		cx := float64(clicked.X) + (float64(rx)+0.5)*float64(clicked.CellW)
		cy := float64(clicked.Y) + (float64(ry)+0.5)*float64(clicked.CellH)
		if !hasNeighbor(cx, cy) {
			return fmt.Errorf("must touch existing brick")
		}
		clicked.Bricks[idx] = 1
		clicked.HP++
		if clicked.HP > clicked.HPMax {
			clicked.HP = clicked.HPMax
		}
	} else {
		bx := (x / BS) * BS
		by := (y / BS) * BS
		if bx < leftBound || bx+BS > rightBound || by < upperLimit || by+BS > cs.GroundPx {
			return fmt.Errorf("out of build area")
		}
		// Reject if the candidate cell [bx, bx+BS) × [by, by+BS) overlaps an
		// alive brick belonging to ANY existing wall. The earlier "clicked"
		// lookup uses half-open bbox bounds and a brick-grid that's aligned
		// to BS, but several initial walls (battlements, partition, throne-
		// ring) sit at non-BS-aligned X coords with W < BS — so a click in
		// the gap next to such a wall used to snap to a BS grid cell that
		// visually overlapped them, producing two stacked bricks at the same
		// screen position. Mirrors the "brick already there" rejection in
		// the clicked branch.
		if cellOverlapsAliveBrick(cs.Walls, bx, by, BS, BS) {
			return fmt.Errorf("brick already there")
		}
		cx := float64(bx) + float64(BS)/2
		cy := float64(by) + float64(BS)/2
		if !hasNeighbor(cx, cy) {
			return fmt.Errorf("must touch existing brick")
		}
		cs.Walls = append(cs.Walls, Wall{
			Kind: "keep-wall", X: bx, Y: by, W: BS, H: BS,
			Cols: 1, Rows: 1, CellW: BS, CellH: BS,
			Bricks: []int8{1}, HP: 1, HPMax: 1,
		})
	}
	cs.PendingBricks--
	return nil
}

// cellOverlapsAliveBrick returns true if the rectangle [cx, cx+cw) × [cy, cy+ch)
// intersects an alive brick in any wall. Used by PlaceBrick (and addSupportRow)
// to prevent a freshly-created 1×1 wall from sitting on top of an existing
// battlement/partition/etc whose grid cells aren't BS-aligned.
//
// Correctness: this is a full AABB-vs-AABB intersection per brick. Sampling
// just the centre of either rectangle is wrong when the wall's cells are
// larger than the candidate (e.g. battlement W=14, H=20 vs candidate BS=10):
// their AABBs can overlap even though neither centre falls inside the other.
func cellOverlapsAliveBrick(walls []Wall, cx, cy, cw, ch int) bool {
	cx2, cy2 := cx+cw, cy+ch
	for i := range walls {
		w := walls[i]
		if w.HP <= 0 {
			continue
		}
		// Cheap rejection by wall bbox.
		if cx2 <= w.X || cx >= w.X+w.W || cy2 <= w.Y || cy >= w.Y+w.H {
			continue
		}
		// Compute the rows/cols of w whose grid-cell AABB can touch the
		// candidate. clamp to [0, w.Cols-1] / [0, w.Rows-1].
		rx0 := (cx - w.X) / w.CellW
		if rx0 < 0 {
			rx0 = 0
		}
		ry0 := (cy - w.Y) / w.CellH
		if ry0 < 0 {
			ry0 = 0
		}
		// (cx2 - 1 - w.X) / w.CellW is the inclusive last column the
		// candidate can hit. Add 1 for the exclusive upper bound.
		rxN := (cx2 - 1 - w.X) / w.CellW
		if rxN >= w.Cols {
			rxN = w.Cols - 1
		}
		ryN := (cy2 - 1 - w.Y) / w.CellH
		if ryN >= w.Rows {
			ryN = w.Rows - 1
		}
		for ry := ry0; ry <= ryN; ry++ {
			gy := w.Y + ry*w.CellH
			if gy >= cy2 || gy+w.CellH <= cy {
				continue
			}
			for rx := rx0; rx <= rxN; rx++ {
				gx := w.X + rx*w.CellW
				if gx >= cx2 || gx+w.CellW <= cx {
					continue
				}
				if w.Bricks[ry*w.Cols+rx] == 1 {
					return true
				}
			}
		}
	}
	return false
}

func (e *Engine) SetTax(pIdx, tax int) error {
	if tax < 0 || tax > 100 {
		return fmt.Errorf("tax must be 0-100")
	}
	e.GS.Castles[pIdx].TaxRate = tax
	return nil
}

func (e *Engine) EndTurnManual(pIdx int) error {
	if e.GS.Phase != "aim" {
		return fmt.Errorf("not in aim phase")
	}
	if pIdx != e.GS.Turn {
		return fmt.Errorf("not your turn")
	}
	return e.endTurn(false)
}

func (e *Engine) Surrender(pIdx int) error {
	if e.GS.Phase != "aim" || e.GS.Winner != nil {
		return fmt.Errorf("cannot surrender now")
	}
	w := 1 - pIdx
	e.GS.Winner = &w
	e.GS.Reason = fmt.Sprintf("Spieler %d gibt auf", pIdx+1)
	e.GS.Phase = "gameover"
	return nil
}

func (e *Engine) endTurn(scored bool) error {
	if e.GS.Phase == "gameover" {
		return nil
	}
	// Economy: tax × pop, plus tower bonus minus tower upkeep. Population
	// drifts based on the tax bracket (higher tax → faster decline) and gains
	// a flat boost equal to the number of working towers.
	for pi := 0; pi < 2; pi++ {
		cs := &e.GS.Castles[pi]
		tf := float64(cs.TaxRate) / 100.0
		income := int(math.Round(float64(cs.Population) * tf * BASE_TAX_RATE * TAX_ROUND_SCALE))
		at := 0
		for _, t := range cs.Towers {
			if t.HP > 0 {
				at++
			}
		}
		tb := at * TOWER_GOLD_BONUS
		up := at * TOWER_UPKEEP
		cs.Gold = max(0, cs.Gold+income+tb-up)

		var delta int
		switch {
		case cs.TaxRate >= 80:
			delta = -11 + rand.Intn(4) // -11..-8
		case cs.TaxRate >= 65:
			delta = -5 + rand.Intn(3) // -5..-3
		case cs.TaxRate >= 45:
			delta = -2 + rand.Intn(4) // -2..+1
		case cs.TaxRate >= 25:
			delta = 2 + rand.Intn(3) // +2..+4
		default:
			delta = 4 + rand.Intn(3) // +4..+6
		}
		delta += at
		cs.Population = clampInt(cs.Population+delta, POP_MIN, POP_MAX)
	}

	// Surrender / loss conditions
	for pi := 0; pi < 2; pi++ {
		cs := e.GS.Castles[pi]
		ac := 0
		for _, c := range cs.Cannons {
			if c.Alive {
				ac++
			}
		}
		if ac == 0 && cs.Gold < PRICE_CANNON && cs.Population < 20 {
			w := 1 - pi
			e.GS.Winner = &w
			e.GS.Reason = fmt.Sprintf("Spieler %d kapituliert", pi+1)
			e.GS.Phase = "gameover"
			return nil
		}
		if cs.Population <= 0 {
			ad := true
			for _, c := range cs.Cannons {
				if c.Alive {
					ad = false
					break
				}
			}
			if ad {
				w := 1 - pi
				e.GS.Winner = &w
				e.GS.Reason = fmt.Sprintf("Spieler %d verloren", pi+1)
				e.GS.Phase = "gameover"
				return nil
			}
		}
	}

	e.GS.Wind = rand.Float64()*3.15 - 1.575
	e.GS.Turn = 1 - e.GS.Turn
	e.GS.Phase = "aim"
	if e.GS.Turn == 0 {
		e.GS.Round++
		if e.GS.Round > MAX_ROUND {
			v0 := cVal(e.GS.Castles[0])
			v1 := cVal(e.GS.Castles[1])
			w := 0
			if v1 > v0 {
				w = 1
			}
			e.GS.Winner = &w
			e.GS.Reason = fmt.Sprintf("Rundenlimit! %d vs %d", v0, v1)
			e.GS.Phase = "gameover"
			return nil
		}
	}
	return nil
}

func (e *Engine) HandleCmd(pIdx int, cmd Cmd) error {
	switch cmd.Action {
	case "fire":
		p, _ := cmd.Payload.(FireP)
		return e.Fire(pIdx, p.Cannon, p.Angle, p.Power, p.AmmoType)
	case "buy":
		p, _ := cmd.Payload.(BuyP)
		return e.Buy(pIdx, p.Item)
	case "end_turn":
		return e.EndTurnManual(pIdx)
	case "set_tax":
		p, _ := cmd.Payload.(TaxP)
		return e.SetTax(pIdx, p.Tax)
	case "place_brick":
		p, _ := cmd.Payload.(PlaceBrickP)
		return e.PlaceBrick(pIdx, p.X, p.Y)
	case "sell_tower":
		p, _ := cmd.Payload.(SellTowerP)
		return e.SellTower(pIdx, p.Index)
	case "surrender":
		return e.Surrender(pIdx)
	default:
		return fmt.Errorf("unknown action: %s", cmd.Action)
	}
}

// ─── Projectile simulation (server-authoritative) ───────────────────────────
//
// Called from the lobby's command-loop goroutine on a 30Hz ticker while
// Phase == "flying". Each call advances SUBSTEPS×2 micro-steps to match the
// original game's 360 physics-steps/sec and broadcasts the new state.

func (e *Engine) StepProjectile() {
	if len(e.GS.Projectiles) == 0 || e.GS.Phase != "flying" {
		return
	}
	scoredAny := false
	for s := 0; s < SUBSTEPS*2; s++ {
		if len(e.GS.Projectiles) == 0 || e.GS.Phase != "flying" {
			break
		}
		survivors := e.GS.Projectiles[:0]
		for _, p := range e.GS.Projectiles {
			prevX, prevY := p.X, p.Y
			p.VY += GRAVITY

			gustDelta := WIND_GUST_DELTA
			gustMax := WIND_GUST_MAX
			switch p.PType {
			case "shrapnel":
				gustDelta = SHRAPNEL_GUST
				gustMax = SHRAPNEL_GUST_MAX

			}
			p.WindGust += (rand.Float64()*2 - 1) * gustDelta
			if p.WindGust > gustMax {
				p.WindGust = gustMax
			}
			if p.WindGust < -gustMax {
				p.WindGust = -gustMax
			}
			p.VX += (e.GS.Wind + p.WindGust) * WIND_C
			p.X += p.VX
			p.Y += p.VY

			// Out of bounds → drop without score
			if p.X < 0 || p.X >= float64(e.GS.W) || p.Y >= H {
				continue
			}
			if s%4 == 0 {
				p.Trail = append(p.Trail, Pt{X: p.X, Y: p.Y})
				if len(p.Trail) > 90 {
					p.Trail = p.Trail[len(p.Trail)-90:]
				}
			}
			if e.checkProjectileCollision(p, prevX, prevY, p.X, p.Y) {
				scoredAny = true
				if e.GS.Phase == "gameover" {
					e.GS.Projectiles = nil
					return
				}
				continue
			}
			survivors = append(survivors, p)
		}
		// zero out the tail of the underlying array so removed projectiles don't leak
		for i := len(survivors); i < len(e.GS.Projectiles); i++ {
			e.GS.Projectiles[i] = nil
		}
		e.GS.Projectiles = survivors
	}
	if len(e.GS.Projectiles) == 0 && e.GS.Phase == "flying" {
		e.endFlying(scoredAny)
	}
}

func (e *Engine) checkProjectileCollision(p *Pro, x0, y0, x1, y1 float64) bool {
	dx := x1 - x0
	dy := y1 - y0
	dist := math.Sqrt(dx*dx + dy*dy)
	steps := int(math.Max(1, math.Ceil(dist/3)))
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		sx := int(x0 + dx*t)
		sy := int(y0 + dy*t)
		if sx < 0 || sx >= e.GS.W {
			continue
		}

		// 1. Walls (must be first — chambers are only reachable through breached walls)
		for ci := 0; ci < 2; ci++ {
			cs := &e.GS.Castles[ci]
			for wi := range cs.Walls {
				if hitW(cs.Walls[wi], sx, sy) {
					e.applyExplosion(p, sx, sy, false, nil)
					return true
				}
			}
		}

		// 2. Cannons. Hitbox is generous and shifted upward to cover the
		// rendered barrel (length 40, default 45° pose) — not just the breech.
		// The firing cannon is exempt: the projectile spawns inside its own
		// hitbox, so without this it would self-destruct on the first step.
		for ci := 0; ci < 2; ci++ {
			cs := &e.GS.Castles[ci]
			for cni := range cs.Cannons {
				if ci == p.OwnerSide && cni == p.OwnerIdx {
					continue
				}
				cn := &cs.Cannons[cni]
				if cn.Alive && absInt(sx-cn.BX) <= 26 && sy >= cn.BY-32 && sy <= cn.BY+16 {
					cn.Alive = false
					// Anchor the blast at the breech, not the airy hitbox
					// impact point. The hitbox extends 32 px above the breech
					// to catch high-arc shots, and a descending shell hits the
					// air half first — exploding there wastes the upper half
					// of the radius on sky and barely scratches the wall below.
					// Centering on (cn.BX, cn.BY) puts the lower half of the
					// blast straight into the supporting structure, which is
					// what players expect from a direct hit (especially with
	
					e.applyExplosion(p, cn.BX, cn.BY, false, nil)
					return true
				}
			}
			// 3. Wind vane. Wide enough to cover the flag triangle in either
			// wind direction, not only the thin pole.
			v := &cs.WindVane
			if v.Alive && absInt(sx-v.X) <= 36 && sy >= v.Y-12 && sy <= v.TowerY {
				v.Alive = false
				// Anchor at the structural junction (top of the vane tower),
				// not the flag tip — same air-bias fix as for cannons.
				e.applyExplosion(p, v.X, v.TowerY, false, nil)
				return true
			}
			// 4. Towers
			for ti := range cs.Towers {
				tw := &cs.Towers[ti]
				if tw.HP > 0 && sx >= tw.X && sx < tw.X+tw.W && sy >= tw.Y && sy < tw.Y+tw.H {
					tw.HP--
					e.applyExplosion(p, sx, sy, false, tw)
					return true
				}
			}
			// 5. Chambers (reachable only after a wall has been carved)
			for chi := range cs.Chambers {
				ch := &cs.Chambers[chi]
				if ch.Alive && sx >= ch.X && sx < ch.X+ch.W && sy >= ch.Y && sy < ch.Y+ch.H {
					ch.Alive = false
					if ch.Kind == "throne" {
						cs.King.Alive = false
						w := 1 - ci
						e.GS.Winner = &w
						e.GS.Reason = fmt.Sprintf("Thron von Spieler %d zerstört", ci+1)
						e.GS.Phase = "gameover"
						e.GS.Projectiles = nil
						return true
					}
					switch ch.Kind {
					case "powder":
						cs.Powder = 0
					case "treasure":
						cs.Gold = 0
					case "ammo":
						cs.Ammo = 0
					}
					e.applyExplosion(p, sx, sy, ch.Kind == "powder", nil)
					return true
				}
			}
		}

		// 6. Terrain
		ty := H - int(e.GS.Terrain.Data[sx])
		if sy >= ty {
			e.applyExplosion(p, sx, ty, false, nil)
			return true
		}
	}
	return false
}

func (e *Engine) applyExplosion(p *Pro, cx, cy int, isPowder bool, skipTower *Twr) {
	rad := EXP_R
	wcr := WCR
	if isPowder {
		rad = EXP_RP
		wcr = WCRP
	} else if p != nil {
		switch p.PType {
		case "shrapnel":
			rad = SHRAPNEL_BLAST_R
			wcr = SHRAPNEL_BLAST_WCR

		}
	}

	// Carve terrain — lower the surface (increase y of ground top) within the disk.
	for x := cx - rad; x <= cx+rad; x++ {
		if x < 0 || x >= e.GS.W {
			continue
		}
		dx := x - cx
		// half-chord at this x
		dy2 := rad*rad - dx*dx
		if dy2 < 0 {
			continue
		}
		dy := int(math.Sqrt(float64(dy2)))
		// new ground TOP y in canvas coords; smaller = higher
		newTopY := cy + dy
		if newTopY > H-1 {
			newTopY = H - 1
		}
		// terrain.data[x] stores GROUND HEIGHT (= H - y_top)
		newHeight := H - newTopY
		if newHeight < 0 {
			newHeight = 0
		}
		if uint16(newHeight) < e.GS.Terrain.Data[x] {
			e.GS.Terrain.Data[x] = uint16(newHeight)
		}
	}

	// Collapse towers whose base is now floating after terrain was carved away.
	for ci := 0; ci < 2; ci++ {
		cs := &e.GS.Castles[ci]
		for ti := range cs.Towers {
			t := &cs.Towers[ti]
			if t.HP <= 0 {
				continue
			}
			midX := clampInt(t.X+t.W/2, 0, e.GS.W-1)
			groundY := H - int(e.GS.Terrain.Data[midX])
			if groundY > t.Y+t.H {
				t.HP = 0
			}
		}
	}

	// Damage walls in radius
	for ci := 0; ci < 2; ci++ {
		cs := &e.GS.Castles[ci]
		for wi := range cs.Walls {
			carveWB(&cs.Walls[wi], cx, cy, wcr)
		}
		// Vane-tower cascade: if any vane-tower wall is now fully carved and the
		// vane is still alive, the flag falls with it.
		if cs.WindVane.Alive {
			for wi := range cs.Walls {
				w := &cs.Walls[wi]
				if w.Kind == "vane-tower" && w.HP <= 0 {
					cs.WindVane.Alive = false
					break
				}
			}
		}
		// Floating-cannon check: any cannon whose supporting roof was just
		// carved away topples with it.
		for cni := range cs.Cannons {
			cn := &cs.Cannons[cni]
			if cn.Alive && !cannonSupported(cs, cn.BX, cn.BY) {
				cn.Alive = false
			}
		}
		// Flatten towers and chambers within explosion radius (all shot types)
		r2 := rad * rad
		// Destroy cannons within the explosion radius (all shot types),
		// not just on a direct hit.
		for cni := range cs.Cannons {
			cn := &cs.Cannons[cni]
			if cn.Alive {
				ddx := cx - cn.BX
				ddy := cy - cn.BY
				if ddx*ddx+ddy*ddy <= r2 {
					cn.Alive = false
				}
			}
		}
		for ti := range cs.Towers {
			t := &cs.Towers[ti]
			if t == skipTower {
				continue
			}
			if t.HP > 0 {
				tcx := t.X + t.W/2
				tcy := t.Y + t.H/2
				ddx := cx - tcx
				ddy := cy - tcy
				if ddx*ddx+ddy*ddy <= r2 {
					t.HP--
				}
			}
		}
		for chi := range cs.Chambers {
			ch := &cs.Chambers[chi]
			if ch.Alive {
				chcx := ch.X + ch.W/2
				chcy := ch.Y + ch.H/2
				ddx := cx - chcx
				ddy := cy - chcy
				chR2 := r2
				if ch.Kind == "throne" {
					tr := rad - THRONE_BLAST_MARGIN
					if tr < 0 {
						tr = 0
					}
					chR2 = tr * tr
				}
				if ddx*ddx+ddy*ddy <= chR2 {
					ch.Alive = false
					switch ch.Kind {
					case "throne":
						cs.King.Alive = false
						w := 1 - ci
						e.GS.Winner = &w
						e.GS.Reason = fmt.Sprintf("Thron von Spieler %d zerstört", ci+1)
						e.GS.Phase = "gameover"
						e.GS.Projectiles = nil
					case "powder":
						cs.Powder = 0
					case "treasure":
						cs.Gold = 0
					case "ammo":
						cs.Ammo = 0
					}
				}
			}
		}
	}

}

func (e *Engine) endFlying(scored bool) {
	e.GS.Projectiles = nil
	if e.GS.Phase == "gameover" {
		return
	}
	e.endTurn(scored)
}
