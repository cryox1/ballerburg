#!/usr/bin/env python3
"""Smoke test: buy Schrapnell and fire it immediately in the same turn.

This validates the command flow used by the cannon popup:

1) buy `buy-shrapnel`
2) wait for `shrapnelammo` to increase in state
3) fire with `ammoType: "shrapnel"` without ending the turn first
4) observe shrapnel projectiles in flight

Usage:
    python3 tests/ai/shrapnel_flow_smoke.py
    BALLERBURG_HOST=100.88.184.113 BALLERBURG_PORT=9981 python3 tests/ai/shrapnel_flow_smoke.py

Requires: pip install --user websocket-client
Exits 0 on pass, non-zero on failure.
"""

import json
import os
import sys
import time
from websocket import WebSocketTimeoutException, create_connection


HOST = os.environ.get("BALLERBURG_HOST", "100.88.184.113")
PORT = int(os.environ.get("BALLERBURG_PORT", "9981"))
URL = f"ws://{HOST}:{PORT}/ws?name=ShrapFlow&team=Sonstiges&ai=easy&ghost=1"


def send(ws, action, payload=None):
    ws.send(json.dumps({"type": "command", "data": {"action": action, "payload": payload or {}}}))


def recv_state(ws, timeout=3.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        ws.settimeout(max(0.2, deadline - time.monotonic()))
        try:
            msg = json.loads(ws.recv())
        except WebSocketTimeoutException:
            return None
        if msg.get("type") == "state":
            return msg["state"]
    return None


def wait_for_state(ws, predicate, timeout, fail_msg):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        st = recv_state(ws, timeout=max(0.5, deadline - time.monotonic()))
        if st is None:
            continue
        last = st
        if predicate(st):
            return st
    if last is None:
        raise RuntimeError(f"{fail_msg} (no state received)")
    raise RuntimeError(f"{fail_msg} (last phase={last.get('phase')} round={last.get('round')})")


def main():
    ws = create_connection(URL, timeout=15)
    hello = json.loads(ws.recv())
    me = hello["you"]
    state = hello["state"]

    try:
        state = wait_for_state(
            ws,
            lambda s: s.get("phase") == "aim" and s.get("turn") == me,
            timeout=20.0,
            fail_msg="never reached my aim turn",
        ) if not (state.get("phase") == "aim" and state.get("turn") == me) else state

        my_castle = state["castles"][me]
        cannon = next((i for i, c in enumerate(my_castle.get("cannons") or []) if c.get("alive")), None)
        if cannon is None:
            raise RuntimeError("no alive cannon available")

        before_shrap = my_castle.get("shrapnelammo", 0)
        before_gold = my_castle.get("gold", 0)
        print(f"before buy: gold={before_gold} shrapnel={before_shrap} cannon={cannon}")

        send(ws, "buy", {"item": "buy-shrapnel"})

        state = wait_for_state(
            ws,
            lambda s: (
                s.get("phase") == "aim"
                and s.get("turn") == me
                and s["castles"][me].get("shrapnelammo", 0) >= before_shrap + 1
            ),
            timeout=8.0,
            fail_msg="buy-shrapnel was not reflected immediately in state",
        )

        my_castle = state["castles"][me]
        after_shrap = my_castle.get("shrapnelammo", 0)
        after_gold = my_castle.get("gold", 0)
        print(f"after buy:  gold={after_gold} shrapnel={after_shrap}")

        send(ws, "fire", {"cannon": cannon, "angle": 45, "power": 30, "ammoType": "shrapnel"})

        state = wait_for_state(
            ws,
            lambda s: s.get("phase") == "flying" and any(
                p.get("ptype") == "shrapnel" for p in (s.get("projectiles") or [])
            ),
            timeout=8.0,
            fail_msg="did not observe shrapnel projectile after immediate fire",
        )

        shrap_count = sum(1 for p in (state.get("projectiles") or []) if p.get("ptype") == "shrapnel")
        print(f"observed shrapnel projectiles: {shrap_count}")
        if shrap_count <= 0:
            raise RuntimeError("expected shrapnel projectiles, got none")

        print("OK")
    finally:
        ws.close()


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(f"FAIL: {exc}")
        sys.exit(2)
