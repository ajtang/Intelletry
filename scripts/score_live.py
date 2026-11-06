#!/usr/bin/env python3
"""Scores a running local pipeline against the simulator's hidden ground truth.

Start the stack with scripts/local.sh up, give faults several minutes to develop,
then run this script. It only reads from the local API and simulator.
"""
import collections
import json
import urllib.request

API = "http://localhost:8090/api/v1"
TOKEN = "local-dev-api-token-0001"
FAULTS = "http://localhost:8081/faults"

EXPECTED = {
    "battery_overheat": "thermal_management_issue",
    "cooling_degradation": "battery_cooling_failure",
    "charging_inefficiency": "charging_anomaly",
    "connectivity_intermittent": "connectivity_instability",
    "software_regression": "abnormal_power_draw",
    "sensor_drift": "sensor_drift",
    "excessive_consumption": "excessive_energy_consumption",
}


def get(url, auth=True):
    req = urllib.request.Request(url)
    if auth:
        req.add_header("Authorization", f"Bearer {TOKEN}")
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.load(resp)


def main():
    alerts = get(f"{API}/alerts?limit=500")["alerts"]
    faults = get(FAULTS, auth=False)

    by_vehicle = collections.defaultdict(set)
    for alert in alerts:
        by_vehicle[alert["subject"]].add(alert["category"])

    totals = collections.Counter(faults.values())
    hits = collections.Counter()
    wrong = collections.Counter()
    for subject, categories in by_vehicle.items():
        if subject.startswith("software:"):
            continue
        fault = faults.get(subject)
        for category in categories:
            if EXPECTED.get(fault) == category:
                hits[fault] += 1
            else:
                wrong[(fault or "healthy", category)] += 1

    print("Live recall by fault")
    for fault in sorted(totals):
        print(f"  {fault:28} {hits[fault]:3}/{totals[fault]}")
    print(f"Alerts not matching a vehicle's injected fault: {sum(wrong.values())}")
    for (fault, category), count in wrong.most_common(10):
        print(f"  {count:3} x {fault} -> {category}")
    print("Fleet-level findings:", [a["subject"] for a in alerts if a["subject"].startswith("software:")])
    print("Status:", dict(collections.Counter(a["status"] for a in alerts)))
    print("Level:", dict(collections.Counter(a["level"] for a in alerts)))

    examples = [a for a in alerts if a["category"] == "battery_cooling_failure"] or alerts
    if examples:
        keys = ["subject", "title", "level", "reason", "likely_cause", "confidence"]
        print(json.dumps({k: examples[0][k] for k in keys}, indent=2))


if __name__ == "__main__":
    main()
