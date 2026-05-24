#!/usr/bin/env python3
"""Behavior smoke test for the Ballerburg AI.

Drives a single-player lobby over WebSocket and asserts:

  1. Hard: AI never buys a 2nd Förderturm in round 1, and never lets gold
     drop below the cannon-replacement reserve (260) while owning a tower
     in round 1. Guards the original "AI buys 2 towers, can't repair cannons"
     bug from regressing.
  2. Easy: AI shots travel toward the human's castle (correct vx sign).
     Catches the old behaviour where Easy shot fully random angles and
     could fire backwards.
  3. Medium: AI never executes a tower-buy that strands it below the 260
     reserve.

Usage:
    # Default: live test stack at 100.88.184.113:9981 (set when ai.go was tuned)
    python3 tests/ai/smoke.py

    # Point at any other deployment by env var
    BALLERBURG_HOST=ubusrv BALLERBURG_PORT=9080 python3 tests/ai/smoke.py
    BALLERBURG_HOST=88.99.84.196 python3 tests/ai/smoke.py  # prod (read-only!)

Requires: pip install --user websocket-client

Exits 0 on pass, non-zero on any failure.
"""

import json
import os
import sys
import time
from websocket import create_connection, WebSocketTimeoutException

HOST = os.environ.get("BALLERBURG_HOST", "100.88.184.113")
PORT = int(os.environ.get("BALLERBURG_PORT", "9981"))
WS_TMPL = "ws://{host}:{port}/ws?name={name}&team=Sonstiges&ai={ai}&ghost=1"

# AI is always slot 1 in single-player lobbies (newAILobby in lobby.go).
AI_SLOT = 1
RESERVE = 260  # PRICE_CANNON — the cannon-replacement reserve baseline


def open_lobby(ai: str, name: str):
    return create_connection(
        WS_TMPL.format(host=HOST, port=PORT, name=name, ai=ai), timeout=15
    )


def send(ws, action, payload=None):
    ws.send(json.dumps({"type": "command", "data": {"action": action, "payload": payload or {}}}))


def is_my_turn(state, my_slot):
    return state and state.get("phase") == "aim" and state.get("turn") == my_slot


def drain_until_my_turn(ws, my_slot, on_state, timeout=30.0):
    """Pull state messages until it's our turn (or gameover/timeout). Calls
    `on_state(state)` for every state message so callers can record metrics."""
    deadline = time.monotonic() + timeout
    last_state = None
    while time.monotonic() < deadline:
        ws.settimeout(max(0.5, deadline - time.monotonic()))
        try:
            raw = ws.recv()
        except WebSocketTimeoutException:
            return last_state
        msg = json.loads(raw)
        if msg.get("type") != "state":
            continue
        state = msg["state"]
        last_state = state
        on_state(state)
        if state.get("phase") == "gameover":
            return state
        if is_my_turn(state, my_slot):
            return state
    return last_state


# ── Test 1: Hard — no premature 2nd tower, gold reserve preserved ──────────

def test_hard_no_double_tower():
    print("[hard] connect…")
    ws = open_lobby("hard", "SmokeH")
    hello = json.loads(ws.recv())
    my_slot = hello["you"]
    state = hello["state"]
    failures = []

    for _ in range(8):  # safety bound
        if state.get("phase") == "gameover" or state["round"] >= 5:
            break
        if not is_my_turn(state, my_slot):
            state = drain_until_my_turn(ws, my_slot, lambda s: None) or state
            if state is None or not is_my_turn(state, my_slot):
                failures.append("never observed my turn")
                break

        send(ws, "end_turn")

        # Track AI's gold/towers throughout its turn.
        max_towers = len(state["castles"][AI_SLOT]["towers"])
        min_gold_with_tower = None

        def track(s):
            nonlocal max_towers, min_gold_with_tower
            ai = s["castles"][AI_SLOT]
            if len(ai["towers"]) > max_towers:
                max_towers = len(ai["towers"])
            if len(ai["towers"]) >= 1:
                if min_gold_with_tower is None or ai["gold"] < min_gold_with_tower:
                    min_gold_with_tower = ai["gold"]

        round_no = state["round"]
        state = drain_until_my_turn(ws, my_slot, track) or state

        ai = state["castles"][AI_SLOT]
        print(f"[hard] after round {round_no}: ai_towers={len(ai['towers'])} "
              f"max_in_round={max_towers} gold={ai['gold']} "
              f"min_w_tower={min_gold_with_tower}")

        if round_no == 1:
            if max_towers >= 2:
                failures.append(f"round 1: AI reached {max_towers} towers (the original bug)")
            if min_gold_with_tower is not None and min_gold_with_tower < RESERVE:
                failures.append(
                    f"round 1: AI gold dropped to {min_gold_with_tower} < {RESERVE} reserve "
                    "while owning a tower (broken cannon-replacement reserve)"
                )

    ws.close()
    return failures


# ── Test 2: Easy — aim points at opponent (correct vx sign) ────────────────

def test_easy_aim_direction():
    print("[easy] connect…")
    ws = open_lobby("easy", "SmokeE")
    hello = json.loads(ws.recv())
    state = hello["state"]
    my_slot = hello["you"]
    failures = []
    saw_ok_projectile = False

    if is_my_turn(state, my_slot):
        send(ws, "end_turn")

    deadline = time.monotonic() + 25.0
    while time.monotonic() < deadline and not saw_ok_projectile:
        ws.settimeout(2.0)
        try:
            raw = ws.recv()
        except WebSocketTimeoutException:
            break
        msg = json.loads(raw)
        if msg.get("type") != "state":
            continue
        state = msg["state"]
        if state.get("phase") == "flying" and state.get("projectiles"):
            for p in state["projectiles"]:
                # AI on slot 1 (right) → its projectiles must travel left (vx < 0).
                if p["vx"] >= 0:
                    failures.append(f"easy AI fired with vx={p['vx']:.2f} (≥0; should be negative)")
                else:
                    saw_ok_projectile = True
        if is_my_turn(state, my_slot) and not saw_ok_projectile:
            send(ws, "end_turn")

    ws.close()
    if not saw_ok_projectile:
        failures.append("never observed an AI projectile on Easy within 25s")
    return failures


# ── Test 3: Medium — never buy a tower below the 260 reserve ───────────────

def test_medium_reserve():
    print("[medium] connect…")
    ws = open_lobby("medium", "SmokeM")
    hello = json.loads(ws.recv())
    state = hello["state"]
    my_slot = hello["you"]
    failures = []
    last_towers = len(state["castles"][AI_SLOT]["towers"])

    for _ in range(8):
        if state.get("phase") == "gameover" or state["round"] >= 5:
            break
        if is_my_turn(state, my_slot):
            send(ws, "end_turn")

        def track(s):
            nonlocal last_towers
            ai = s["castles"][AI_SLOT]
            if len(ai["towers"]) > last_towers and ai["gold"] < RESERVE:
                failures.append(f"medium AI bought a tower leaving gold={ai['gold']} (<{RESERVE})")
            last_towers = len(ai["towers"])

        state = drain_until_my_turn(ws, my_slot, track) or state

    ws.close()
    return failures


# ── Test 4: AI never fires with powder below the safe floor ───────────────
#
# Regression guard for "AI fires a doomed low-powder shot that lands on its
# own castle". ai.go now (a) refills powder whenever it dips below 50 and
# (b) end_turn's instead of firing when maxPower < 25. So at no point during
# play should the AI's pre-fire powder be < 25 — either it refilled, or it
# ended the turn. We snapshot the AI's powder every time we see it in aim
# phase, then on the next state where a slot-1 projectile appears we assert
# the most-recent aim-phase powder was ≥ 25.

POWDER_FLOOR = 25


def test_ai_never_fires_low_powder():
    print("[powder-floor] connect…")
    ws = open_lobby("medium", "SmokeP")
    hello = json.loads(ws.recv())
    state = hello["state"]
    my_slot = hello["you"]
    failures = []
    last_aim_powder = None  # AI's powder during its most recent aim-phase state
    fires_observed = 0

    deadline = time.monotonic() + 60.0
    while time.monotonic() < deadline and fires_observed < 6:
        if is_my_turn(state, my_slot):
            send(ws, "end_turn")
        ws.settimeout(2.0)
        try:
            raw = ws.recv()
        except WebSocketTimeoutException:
            break
        msg = json.loads(raw)
        if msg.get("type") != "state":
            continue
        state = msg["state"]
        if state.get("phase") == "gameover":
            break
        ai_powder = state["castles"][AI_SLOT]["powder"]
        if state.get("phase") == "aim" and state.get("turn") == AI_SLOT:
            last_aim_powder = ai_powder
        if state.get("phase") == "flying" and state.get("projectiles"):
            for p in state["projectiles"]:
                # AI is on slot 1 (right) → its projectiles travel left.
                if p["vx"] < 0:
                    fires_observed += 1
                    if last_aim_powder is not None and last_aim_powder < POWDER_FLOOR:
                        failures.append(
                            f"AI fired with pre-shot powder={last_aim_powder} "
                            f"(<{POWDER_FLOOR}); should have refilled or ended turn"
                        )
                    last_aim_powder = None  # avoid double-counting one fire

    ws.close()
    if fires_observed == 0:
        failures.append("never observed an AI fire within 60s — test inconclusive")
    else:
        print(f"[powder-floor] saw {fires_observed} AI fires, all with powder ≥ {POWDER_FLOOR}")
    return failures


def main():
    print(f"target ws://{HOST}:{PORT}/ws  (override via BALLERBURG_HOST/PORT)\n")
    all_fails = []
    for name, fn in (
        ("hard_no_double_tower", test_hard_no_double_tower),
        ("easy_aim_direction", test_easy_aim_direction),
        ("medium_reserve", test_medium_reserve),
        ("ai_never_fires_low_powder", test_ai_never_fires_low_powder),
    ):
        print(f"=== {name} ===")
        try:
            fails = fn()
        except Exception as e:
            fails = [f"{name} crashed: {e!r}"]
        if fails:
            print(f"  FAIL ({len(fails)})")
            for f in fails:
                print("   -", f)
            all_fails.extend((name, f) for f in fails)
        else:
            print("  OK")
        print()

    print("=== summary ===")
    if all_fails:
        for name, f in all_fails:
            print(f"FAIL {name}: {f}")
        sys.exit(1)
    print("All tests passed.")


if __name__ == "__main__":
    main()
