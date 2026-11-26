#!/usr/bin/env python3
"""Deterministic scheduling model, not production or Go execution evidence.

Two cold requests arrive at t=0; four existing warms arrive at t=1.
Clone task/configuration takes 30s, task+reliable host accounting 18s,
and readiness 60s from power POST. Host capacity permits two outstanding
starts. There is no GitHub assignment model or future job-memory claim.
"""
import json

LIMIT, INTERVAL, CLONE, ACCOUNTED, READY = 2, 15, 30, 18, 60


def replay(priority):
    requests = [dict(name=f"cold{i+1}", arrival=0, clone=True) for i in range(2)]
    requests += [dict(name=f"warm{i+1}", arrival=1, clone=False) for i in range(4)]
    slots, starts, clones = [], {}, {}
    next_power = next_clone = 0
    max_outstanding = 0
    for now in range(301):
        slots = [until for until in slots if until > now]
        # Cold preparation used to consume the same slots as warm booting.
        for r in requests:
            if not r["clone"] or r["arrival"] > now or r["name"] in clones:
                continue
            if priority:
                if now < next_clone:
                    continue
                next_clone = now + INTERVAL
            elif len(slots) >= LIMIT:
                continue
            else:
                slots.append(10**6)  # owned until this clone reaches readiness
            clones[r["name"]] = now
            r["power_at"] = now + CLONE
        eligible = [r for r in requests if r["name"] not in starts
                    and r["arrival"] <= now
                    and (not r["clone"] or r["name"] in clones
                         and r["power_at"] <= now)]
        if priority:
            eligible.sort(key=lambda r: (r["clone"], r["arrival"], r["name"]))
        for r in eligible:
            if not priority and now < next_power:
                continue
            if priority and len(slots) >= LIMIT:
                continue
            if not priority and not r["clone"] and len(slots) >= LIMIT:
                continue
            if priority and r["clone"] and now < next_power:
                continue
            starts[r["name"]] = now
            if priority:
                slots.append(now + ACCOUNTED)
                if r["clone"]:
                    next_power = now + INTERVAL
            else:
                if r["clone"]:
                    slots.remove(10**6)
                slots.append(now + READY)
                next_power = now + INTERVAL
            max_outstanding = max(max_outstanding, len(slots))
        if len(starts) == len(requests):
            break
    return dict(clone_posts=clones, power_posts=starts,
                max_outstanding=max_outstanding,
                last_warm_power=max(v for k, v in starts.items() if k.startswith("warm")))


if __name__ == "__main__":
    old, new = replay(False), replay(True)
    assert old["last_warm_power"] == 165
    assert new["last_warm_power"] == 19
    assert new["max_outstanding"] <= LIMIT
    assert new["clone_posts"]["cold2"] - new["clone_posts"]["cold1"] >= INTERVAL
    print(json.dumps({"assumed_seconds": dict(clone=CLONE, accounted=ACCOUNTED,
          ready=READY, interval=INTERVAL), "baseline_model": old,
          "priority_model": new}, indent=2))
