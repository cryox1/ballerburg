# Ballerburg

A turn-based artillery / castle-defense web game — a modern remake of the
classic German *Ballerburg*. Two players each command a castle with cannons,
towers, walls, and a throne, and try to destroy the opponent by lobbing
cannonballs over destructible terrain.

Non-profit / educational project. Full German-language UI.

## Play

- Live: <https://ballern.wut.tf>
- Pick a name & faculty on the index page, then start a game (multiplayer lobby
  code or single-player vs Computer).

## Features

- **WebSocket multiplayer** for two players via 6-character lobby codes, plus
  single-player vs AI (Leicht / Mittel / Schwer).
- **Two ammo types**, selectable in the cannon popup:
  - **Normal** — 1 ball, 120 px blast crater, 60 px wall-carving radius.
  - **Schrapnell** (100 G) — 4 fragments with random drift and wider wind
    variance; 38 px craters; effective at picking off cannons and the wind vane.
- **Wind system** with mid-flight gusts. The wind vane shows exact wind; lose
  it and you only see an estimate (shown with `?`).
- **Economy**: adjustable tax rate (0–100 %), population dynamics, Förderturm
  income towers (max 2, sellable for half price), market for powder / ammo /
  bricks / cannons / vane / towers / Schrapnell.
- **Terrain destruction**, wall reparation by placing bricks.
- **Powder chamber detonation** — hitting a powder chamber drains all reserves
  and triggers a wider secondary blast.
- **Surrender** — dedicated button ends the game immediately.
- **Zoom & pan** — scroll wheel up to 5×, drag to pan, double-click to reset;
  pinch-to-zoom on mobile.
- **Mobile responsive** — landscape sidebar layout / portrait stack; 44 px
  minimum touch targets.
- **Remember device** — name, faculty, and AI difficulty saved to
  `localStorage`.
- **Faculty / team system** — Wirtschaftsinformatik · Informatik · Sonstiges;
  a player name is permanently tied to the first faculty they play with.
- **Leaderboard** — team standings + per-player rankings + per-AI-difficulty
  (Leicht / Mittel / Schwer) tabs on `scoreboard.html`.
- **Tutorial page** explaining all mechanics (`tutorial.html`).

## Win Conditions

| Condition | Description |
|---|---|
| **Throne strike** | Projectile lands within blast radius of the throne chamber → instant win |
| **Capitulation** | All cannons dead + insufficient gold to buy a new one + population < 20 |
| **Round limit** | After round 40 the player with the higher total castle value wins (gold + cannons + ammo + population) |

## AI Difficulties

| Difficulty | Behaviour |
|---|---|
| **Leicht** | Closed-form ballistic aim + large Gaussian noise (±18° / ±10 power); makes no purchases |
| **Mittel** | Closed-form ballistic aim, wind ignored; small noise (±8° / ±4); repairs shields, buys powder/bricks when low |
| **Schwer** | Grid-search over all (angle, power) pairs with Monte Carlo wind sampling (5 samples per cell); pressure-based target weighting (throne/cannon emphasis shifts based on who is winning); buys Schrapnell when opponent has ≥2 cannons |

All AI difficulties observe the reserve system — they hold back gold for cannon
revives, powder, and tower upkeep before spending on anything else.

## Architecture

```
ballerburg/
├── server/                Go backend
│   ├── engine.go          game state, fire/buy/step physics, collisions, explosions
│   ├── ai.go              priority-based AI (powder → ammo → cannon revive → tax → maintenance → fire)
│   ├── lobby.go           per-lobby command loop & AI scheduling, AIAction status
│   ├── wshandler.go       WebSocket framing & command dispatch
│   ├── scoreboard.go      SQLite persistence (multiplayer wins only)
│   ├── admin.go           admin dashboard handlers
│   └── ws.go, main.go     low-level WS + HTTP server
├── game.html              client (Canvas + WebSocket)
├── tutorial.html          rules & strategy
├── index.html             lobby/login
├── scoreboard.html        public stats
├── nginx.conf             frontend reverse proxy (proxies /ws to backend)
├── Dockerfile.backend     Go multi-stage build → distroless-style alpine runtime
├── Dockerfile.frontend    nginx:alpine + static HTML
└── docker-compose.yml     `frontend` (9080) + `backend` (9092)
```

Server is authoritative for all game state — clients only render and forward
input. Each lobby owns one goroutine that holds the engine; commands are
serialised through a channel.

## Local Development

```bash
docker compose build
docker compose up
# open http://localhost:9080
```

The backend's WebSocket endpoint is `ws://localhost:9080/ws?name=...&team=...`
(the frontend container reverse-proxies `/ws` to `backend:9092`).

To run the Go server natively (no Docker):
```bash
cd server
go run .
# WebSocket: ws://localhost:9092/ws
```

## Deployment

Build and push to any Docker host:

```bash
rsync -avz -e "ssh -i ~/.ssh/id_ubusrv" --exclude='ballerburg-server' \
  server/ wh@<host>:/home/wh/ballerburg/server/
rsync -avz -e "ssh -i ~/.ssh/id_ubusrv" *.html nginx.conf Dockerfile.* \
  wh@<host>:/home/wh/ballerburg/
ssh -i ~/.ssh/id_ubusrv wh@<host> \
  'cd /home/wh/ballerburg && docker compose build backend frontend && docker compose up -d'
```

**Gotcha**: when adding new frontend files (e.g. a new `.html` page), make
sure it's listed in `COPY` lines in `Dockerfile.frontend` — files on the host
are not volume-mounted into the nginx container.

## Constants worth knowing

**Starting resources**

| Constant | Value | Meaning |
|---|---|---|
| `INIT_GOLD` | 500 | starting gold |
| `INIT_POWDER` / `INIT_AMMO` | 280 / 15 | starting consumables |
| `INIT_POP` | 120 | starting population |

**Economy**

| Constant | Value | Meaning |
|---|---|---|
| `BASE_TAX_RATE` × `TAX_ROUND_SCALE` | 2.3 × 0.09 | per-round tax-income scale |
| `TOWER_GOLD_BONUS` − `TOWER_UPKEEP` | 24 − 8 = +16/round | net Förderturm income |
| `MAX_ROUND` | 40 | round limit (tiebreak by total castle value) |

**Market prices**

| Item | Price |
|---|---|
| 50× Pulver | 50 G |
| 5× Munition | 20 G |
| Schrapnell (1 shot) | 100 G |
| 20× Mauersteine | 90 G |
| Kanone | 260 G |
| Wetterfahne | 120 G |
| Förderturm (sell: 110 G) | 220 G |

**Blast radii**

| Constant | Value | Meaning |
|---|---|---|
| `EXP_R` / `WCR` | 120 / 60 | normal-shot crater & brick-kill radii |
| `SHRAPNEL_BLAST_R` / `SHRAPNEL_BLAST_WCR` | 38 / 27 | Schrapnell radii (per fragment) |

## License

See [LICENSE](LICENSE).
