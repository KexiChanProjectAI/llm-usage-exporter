#!/usr/bin/env python3
"""Generate the Grafana dashboard for llm-usage-exporter.

    python3 gen_dashboard.py OUT.json [--datasource-uid UID] [--title TITLE] [--job JOB]

The dashboard has no template variables so that it can be shared as a
Grafana public dashboard, which does not support them.
"""
import argparse, json

ap = argparse.ArgumentParser()
ap.add_argument("out")
ap.add_argument("--datasource-uid", default="prometheus", help="uid of the Prometheus datasource")
ap.add_argument("--title", default="LLM quota")
ap.add_argument("--job", default="llm_usage", help="Prometheus job name of the exporter")
ap.add_argument("--uid", default="llm-usage", help="dashboard uid")
args = ap.parse_args()

DS = {"type": "prometheus", "uid": args.datasource_uid}
SEL = f'job="{args.job}"'
BY = "provider, account_id, account_name"
STATE = f'(max by ({BY}) (llm_quota_key_blacklisted{{{SEL}}}) == 1) * 2 or max by ({BY}) (llm_quota_up{{{SEL}}})'
STATE_MAP = [{"type": "value", "options": {
    "0": {"text": "✕ Failing", "color": "red", "index": 0}, "1": {"text": "✓ OK", "color": "green", "index": 1},
    "2": {"text": "⊘ Blacklisted (401)", "color": "orange", "index": 2}}}]
pid = [0]
def nid():
    pid[0] += 1; return pid[0]

THRESH = {"mode": "absolute", "steps": [
    {"color": "green", "value": None}, {"color": "yellow", "value": 0.7}, {"color": "red", "value": 0.9}]}

def tgt(expr, ref, legend="", instant=False, fmt="time_series"):
    t = {"datasource": DS, "expr": expr, "refId": ref, "legendFormat": legend, "format": fmt}
    if instant: t.update(instant=True, range=False)
    else: t.update(range=True)
    return t

def stat(title, expr, x, y, w, unit="none", thresholds=None, desc="", no_value="–", decimals=None):
    d = {"unit": unit, "noValue": no_value, "thresholds": thresholds or
            {"mode": "absolute", "steps": [{"color": "text", "value": None}]}, "color": {"mode": "thresholds"}}
    if decimals is not None: d["decimals"] = decimals
    return {"type": "stat", "title": title, "description": desc, "datasource": DS, "id": nid(),
        "gridPos": {"h": 3, "w": w, "x": x, "y": y},
        "targets": [tgt(expr, "A", instant=True)],
        "fieldConfig": {"defaults": d, "overrides": []},
        "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
            "colorMode": "value", "graphMode": "none", "textMode": "value", "justifyMode": "center"}}

def row(title, y):
    return {"type": "row", "title": title, "id": nid(), "collapsed": False, "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "panels": []}

LEGEND = "{{provider}} / {{account_name}}"
SERIES = {"lineWidth": 2, "fillOpacity": 0, "showPoints": "never", "spanNulls": 600000}

def ratio_ts(title, window, x, y, w, time_from=None, no_value="No data"):
    p = {"type": "timeseries", "title": title, "datasource": DS, "id": nid(),
        "description": f"Share of the {window} window used, per account. Dashed lines mark 70% and 90%.",
        "gridPos": {"h": 8, "w": w, "x": x, "y": y},
        "targets": [tgt(f'max by ({BY}) (llm_quota_used_ratio{{window="{window}", {SEL}}})', "A", LEGEND)],
        "fieldConfig": {"defaults": {"unit": "percentunit", "min": 0, "decimals": 0, "noValue": no_value,
            "color": {"mode": "palette-classic-by-name"}, "thresholds": THRESH,
            "custom": dict(SERIES, thresholdsStyle={"mode": "dashed"}, axisSoftMax=1)}, "overrides": []},
        "options": {"legend": {"displayMode": "table", "placement": "right", "showLegend": True, "calcs": ["lastNotNull", "max"]},
            "tooltip": {"mode": "multi", "sort": "desc"}}}
    if time_from: p["timeFrom"] = time_from
    return p

RED1 = {"mode": "absolute", "steps": [{"color": "green", "value": None}, {"color": "red", "value": 1}]}
panels = []
y = 0
panels += [
    stat("Accounts tracked", f'count(llm_quota_account_info{{{SEL}}})', 0, y, 4,
         desc="sub2api accounts with a supported quota window."),
    stat("Highest usage (any window)", f'max(llm_quota_used_ratio{{{SEL}}})', 4, y, 5, unit="percentunit",
         thresholds=THRESH, decimals=0, desc="Most-used window (5h, weekly or monthly) across all accounts."),
    stat("Fetches failing", f'sum((llm_quota_up{{{SEL}}} == bool 0) * (1 - llm_quota_key_blacklisted{{{SEL}}}))', 9, y, 5,
         thresholds=RED1, desc="Accounts whose last quota fetch failed, excluding blacklisted keys."),
    stat("Keys blacklisted (401)", f'sum(llm_quota_key_blacklisted{{{SEL}}})', 14, y, 5,
         thresholds={"mode": "absolute", "steps": [{"color": "green", "value": None}, {"color": "orange", "value": 1}]},
         desc="Keys rejected with 401; skipped for 6h or until the key changes in sub2api."),
    {**stat("sub2api DB", f'max(llm_exporter_db_up{{{SEL}}})', 19, y, 5,
         desc="Whether the exporter could read accounts from the sub2api database on its last reload."),
     "fieldConfig": {"defaults": {"noValue": "✕ Exporter down", "color": {"mode": "thresholds"},
        "thresholds": {"mode": "absolute", "steps": [{"color": "red", "value": None}, {"color": "green", "value": 1}]},
        "mappings": [{"type": "value", "options": {"0": {"text": "✕ Reload failing", "index": 0}, "1": {"text": "✓ OK", "index": 1}}}]},
        "overrides": []}},
]
y += 3

q = lambda e, r: tgt(e, r, instant=True, fmt="table")
pct_cols = "/^(5h|Weekly|Monthly) used$/"
table = {"type": "table", "title": "Current quota", "datasource": DS, "id": nid(),
    "description": "One row per account. 'Resets in' is the time left until the window resets; 'Data age' is how old the numbers are.",
    "gridPos": {"h": 8, "w": 24, "x": 0, "y": y},
    "targets": [
        q(f'max by ({BY}, status, schedulable) (llm_quota_account_info{{{SEL}}})', "K"),
        q(STATE, "I"),
        q(f'max by ({BY}) (llm_quota_used_ratio{{window="5h", {SEL}}})', "A"),
        q(f'max by ({BY}) (llm_quota_reset_timestamp_seconds{{window="5h", {SEL}}}) - time()', "B"),
        q(f'max by ({BY}) (llm_quota_used_ratio{{window="weekly", {SEL}}})', "C"),
        q(f'max by ({BY}) (llm_quota_reset_timestamp_seconds{{window="weekly", {SEL}}}) - time()', "D"),
        q(f'max by ({BY}) (llm_quota_used_ratio{{window="monthly", {SEL}}})', "E"),
        q(f'max by ({BY}) (llm_quota_reset_timestamp_seconds{{window="monthly", {SEL}}}) - time()', "F"),
        q(f'max by ({BY}) (llm_quota_balance{{kind="total", {SEL}}})', "G"),
        q(f'time() - max by ({BY}) (llm_quota_last_success_timestamp_seconds{{{SEL}}})', "H"),
        q(f'max by ({BY}) (llm_quota_last_http_status{{{SEL}}})', "L"),
        q(f'max by ({BY}, plan) (llm_quota_plan_info{{{SEL}}})', "J"),
    ],
    "transformations": [
        {"id": "merge", "options": {}},
        {"id": "organize", "options": {
            "excludeByName": {"Time": True, "Value #K": True, "Value #J": True},
            "indexByName": {"provider": 0, "account_name": 1, "Value #I": 2, "Value #A": 3, "Value #B": 4,
                "Value #C": 5, "Value #D": 6, "Value #E": 7, "Value #F": 8, "Value #G": 9, "plan": 10,
                "Value #H": 11, "Value #L": 12, "status": 13, "schedulable": 14, "account_id": 15},
            "renameByName": {"provider": "Provider", "account_name": "Account", "account_id": "ID", "plan": "Plan",
                "status": "sub2api status", "schedulable": "Schedulable",
                "Value #I": "Fetch", "Value #A": "5h used", "Value #B": "5h resets in", "Value #C": "Weekly used",
                "Value #D": "Weekly resets in", "Value #E": "Monthly used", "Value #F": "Monthly resets in",
                "Value #G": "Balance (USD)", "Value #H": "Data age", "Value #L": "HTTP"}}},
    ],
    "fieldConfig": {"defaults": {"custom": {"align": "auto", "cellOptions": {"type": "auto"}}, "noValue": "–"},
        "overrides": [
            {"matcher": {"id": "byRegexp", "options": pct_cols}, "properties": [
                {"id": "unit", "value": "percentunit"}, {"id": "decimals", "value": 0}, {"id": "min", "value": 0}, {"id": "max", "value": 1},
                {"id": "thresholds", "value": THRESH}, {"id": "color", "value": {"mode": "thresholds"}},
                {"id": "custom.cellOptions", "value": {"type": "gauge", "mode": "basic", "valueDisplayMode": "text"}},
                {"id": "custom.minWidth", "value": 110}]},
            {"matcher": {"id": "byRegexp", "options": "/resets in$/"}, "properties": [
                {"id": "unit", "value": "dtdurations"},
                {"id": "mappings", "value": [{"type": "range", "options": {"from": -1e12, "to": 0, "result": {"text": "reset passed (stale)", "color": "orange", "index": 0}}}]},
                {"id": "custom.cellOptions", "value": {"type": "color-text"}},
                {"id": "color", "value": {"mode": "fixed", "fixedColor": "text"}}]},
            {"matcher": {"id": "byName", "options": "Balance (USD)"}, "properties": [{"id": "unit", "value": "currencyUSD"}, {"id": "decimals", "value": 2}]},
            {"matcher": {"id": "byName", "options": "Data age"}, "properties": [
                {"id": "unit", "value": "s"}, {"id": "decimals", "value": 0},
                {"id": "thresholds", "value": {"mode": "absolute", "steps": [{"color": "text", "value": None}, {"color": "orange", "value": 900}, {"color": "red", "value": 1800}]}},
                {"id": "color", "value": {"mode": "thresholds"}}, {"id": "custom.cellOptions", "value": {"type": "color-text"}}]},
            {"matcher": {"id": "byName", "options": "Fetch"}, "properties": [
                {"id": "mappings", "value": STATE_MAP}, {"id": "custom.cellOptions", "value": {"type": "color-text"}}]},
            {"matcher": {"id": "byName", "options": "ID"}, "properties": [{"id": "custom.width", "value": 60}]},
        ]},
    "options": {"showHeader": True, "cellHeight": "sm", "sortBy": [{"displayName": "Fetch", "desc": False}]}}
panels.append(table); y += 8

panels.append(row("History", y)); y += 1
panels.append(ratio_ts("5h window used (last 2 days)", "5h", 0, y, 24, time_from="2d")); y += 8
panels += [ratio_ts("Weekly window used", "weekly", 0, y, 12), ratio_ts("Monthly window used", "monthly", 12, y, 12, no_value="No account here has a monthly window")]
y += 8
panels.append({"type": "state-timeline", "title": "Fetch status", "datasource": DS, "id": nid(),
    "description": "Per-account fetch result over time.",
    "gridPos": {"h": 6, "w": 12, "x": 0, "y": y},
    "targets": [tgt(STATE, "A", LEGEND)],
    "fieldConfig": {"defaults": {"color": {"mode": "fixed", "fixedColor": "text"}, "mappings": STATE_MAP,
        "custom": {"fillOpacity": 80, "lineWidth": 0, "spanNulls": 600000}}, "overrides": []},
    "options": {"showValue": "never", "mergeValues": True, "rowHeight": 0.8,
        "legend": {"showLegend": True, "displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "single"}}})
panels.append({"type": "timeseries", "title": "Credit balance (USD)", "datasource": DS, "id": nid(),
    "description": "Total credit balance; only Command Code and Cline report one.",
    "gridPos": {"h": 6, "w": 12, "x": 12, "y": y},
    "targets": [tgt(f'max by ({BY}) (llm_quota_balance{{kind="total", {SEL}}})', "A", LEGEND)],
    "fieldConfig": {"defaults": {"unit": "currencyUSD", "min": 0, "noValue": "No provider here reports a balance",
        "color": {"mode": "palette-classic-by-name"}, "custom": SERIES}, "overrides": []},
    "options": {"legend": {"displayMode": "list", "placement": "bottom", "showLegend": True}, "tooltip": {"mode": "multi", "sort": "desc"}}})
y += 6

dash = {"uid": args.uid, "title": args.title, "tags": ["llm", "sub2api", "quota"],
    "timezone": "browser", "schemaVersion": 41, "version": 2, "editable": True, "graphTooltip": 1,
    "refresh": "5m", "time": {"from": "now-7d", "to": "now"},
    "templating": {"list": []},
    "annotations": {"list": []}, "links": [], "panels": panels}
with open(args.out, "w") as f:
    json.dump(dash, f, indent=2, ensure_ascii=False)
    f.write("\n")
