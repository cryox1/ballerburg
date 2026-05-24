#!/usr/bin/env python3
"""Verify the Hard AI buys and uses Schrapnell when the opponent keeps both
cannons alive. Drives 6+ rounds, end-turning every time it's the human's turn,
and confirms `castles[1].shrapnelammo` rises and at least one shrapnel
projectile is observed travelling toward the human.

Usage:
    python3 tests/ai/shrapnel.py
    BALLERBURG_HOST=ubusrv BALLERBURG_PORT=9981 python3 tests/ai/shrapnel.py

Requires: pip install --user websocket-client
Exits 0 on pass, non-zero on failure.
"""

import json
import os
import sys
import time
from websocket import create_connection, WebSocketTimeoutException

HOST = os.environ.get("BALLERBURG_HOST", "100.88.184.113")
PORT = int(os.environ.get("BALLERBURG_PORT", "9981"))
URL = f"ws://{HOST}:{PORT}/ws?name=ShrapTest&team=Sonstiges&ai=hard&ghost=1"


def main():
    ws = create_connection(URL, timeout=15)
    hello = json.loads(ws.recv())
    my_slot = hello["you"]
    state = hello["state"]

    max_shrap = 0
    saw_fire = False
    last_round_logged = 0

    for _ in range(80):
        if state.get("phase") == "gameover" or state["round"] > 8:
            break
        if state.get("turn") == my_slot and state.get("phase") == "aim":
            ws.send(json.dumps({"type": "command",
                                "data": {"action": "end_turn", "payload": {}}}))
        try:
            ws.settimeout(3.0)
            msg = json.loads(ws.recv())
        except WebSocketTimeoutException:
            break
        if msg.get("type") != "state":
            continue
        state = msg["state"]
        ai = state["castles"][1]
        if ai["shrapnelammo"] > max_shrap:
            max_shrap = ai["shrapnelammo"]
            print(f"  round {state['round']}: AI shrapnelammo→{max_shrap} "
                  f"(gold={ai['gold']}, towers={len(ai['towers'])})")
        for p in state.get("projectiles") or []:
            if p.get("ptype") == "shrapnel" and p.get("vx", 0) < 0:
                saw_fire = True
        if state["round"] != last_round_logged:
            last_round_logged = state["round"]
            print(f"round {state['round']}: ai gold={ai['gold']}, "
                  f"towers={len(ai['towers'])}, shrap_stock={ai['shrapnelammo']}")

    ws.close()
    print(f"\nmax_shrap_stock={max_shrap}  saw_shrapnel_fire={saw_fire}")
    if max_shrap == 0:
        print("FAIL: AI never bought shrapnel within 8 rounds")
        sys.exit(2)
    if not saw_fire:
        print("WARN: bought shrapnel but never observed it firing")
        sys.exit(3)
    print("OK")


if __name__ == "__main__":
    main()
