#!/usr/bin/env python3
"""Generate the Grafana dashboard for llm-usage-exporter.

    python3 gen_dashboard.py OUT.json [--datasource-uid UID] [--title TITLE]
        [--instance NAME] [--prometheus URL | --providers zhipu=8,kimi=4]

The first screen is a status strip plus one table per provider, each sized
to its number of accounts so that every account is visible without
scrolling; history and details follow below.

Table sizes come from --providers, or from a live Prometheus when
--prometheus is given (it counts llm_quota_account_info per provider).
Without either, every provider gets a table with room for 3 accounts.
More accounts than that still show, with a scroll bar inside the table;
regenerate to resize.

Below the quota tables, a usage section charts tokens and their list-price
value per provider from sub2api usage_logs: a table over each window in
--usage-windows and stacked bars for the last 24 hours and the last 7 days.

The dashboard has no template variables so that it can be shared as a
Grafana public dashboard, which does not support them.
"""
import argparse, json, math, urllib.parse, urllib.request

ap = argparse.ArgumentParser()
ap.add_argument("out")
ap.add_argument("--datasource-uid", default="prometheus", help="uid of the Prometheus datasource")
ap.add_argument("--title", default="LLM quota")
ap.add_argument("--job", default="llm_usage", help="Prometheus job name of the exporter")
ap.add_argument("--uid", default="llm-usage", help="dashboard uid")
ap.add_argument("--refresh", default="30s", help="auto-refresh interval")
ap.add_argument("--instance", default="", help="only show the exporter whose config sets this instance (sub2api_instance label)")
ap.add_argument("--providers", default="", help="provider=accounts list, e.g. zhipu=8,kimi=4; sets which tables exist and their height")
ap.add_argument("--prometheus", default="", help="Prometheus base URL to count accounts per provider instead of --providers")
ap.add_argument("--usage-windows", default="1,7,30", help="usage.windows of the exporter (days), for the usage table; empty hides the usage section")
args = ap.parse_args()

DS = {"type": "prometheus", "uid": args.datasource_uid}
SEL = f'job="{args.job}"' + (f', sub2api_instance="{args.instance}"' if args.instance else "")
BY = "provider, account_id, account_name"
pid = [0]
def nid():
    pid[0] += 1; return pid[0]

# Provider sections in display order: (id, title, windows, balance currencies).
PROVIDERS = [
    ("zhipu", "GLM", ["5h", "weekly"], []),
    ("kimi", "Kimi", ["5h", "weekly", "monthly"], []),
    ("minimax", "MiniMax", ["5h", "weekly"], []),
    ("opencode_go", "OpenCode Go", ["5h", "weekly", "monthly"], []),
    ("command_code", "Command Code", ["5h", "weekly", "monthly"], ["USD"]),
    ("cline", "Cline", ["5h", "weekly", "monthly"], ["USD"]),
    ("deepseek", "DeepSeek", [], ["CNY", "USD"]),
]
WINDOW_TITLE = {"5h": "5h", "weekly": "Weekly", "monthly": "Monthly"}

def account_counts():
    if args.prometheus:
        q = f'count by (provider) (llm_quota_account_info{{{SEL}}})'
        url = args.prometheus.rstrip("/") + "/api/v1/query?" + urllib.parse.urlencode({"query": q})
        with urllib.request.urlopen(url, timeout=15) as r:
            res = json.load(r)["data"]["result"]
        return {x["metric"]["provider"]: int(float(x["value"][1])) for x in res}
    if args.providers:
        out = {}
        for part in args.providers.split(","):
            k, _, v = part.partition("=")
            out[k.strip()] = int(v or 3)
        return out
    return {p[0]: 3 for p in PROVIDERS}

COUNTS = account_counts()
unknown = set(COUNTS) - {p[0] for p in PROVIDERS}
if unknown:
    raise SystemExit(f"unknown providers: {', '.join(sorted(unknown))}")

STATE = (f'(max by ({BY}) (llm_quota_key_blacklisted{{{SEL}}}) == 1) * 2'
         f' or (max by ({BY}) (llm_quota_endpoint_supported{{{SEL}}}) == 0) + 3'
         f' or max by ({BY}) (llm_quota_up{{{SEL}}})')
STATE_MAP = [{"type": "value", "options": {
    "0": {"text": "✕ Failing", "color": "red", "index": 0}, "1": {"text": "✓ OK", "color": "green", "index": 1},
    "2": {"text": "⊘ Key rejected", "color": "orange", "index": 2},
    "3": {"text": "– No API", "color": "#6e7681", "index": 3}}}]

# Used share: green while there is room, yellow from 70%, red from 90%.
THRESH = {"mode": "absolute", "steps": [
    {"color": "green", "value": None}, {"color": "yellow", "value": 0.7}, {"color": "red", "value": 0.9}]}

def tgt(expr, ref, legend="", instant=False, fmt="time_series"):
    t = {"datasource": DS, "expr": expr, "refId": ref, "legendFormat": legend, "format": fmt}
    if instant: t.update(instant=True, range=False)
    else: t.update(range=True)
    return t

def tq(expr, ref):
    return tgt(expr, ref, instant=True, fmt="table")

def table_height(rows):
    # Grafana grid unit = 30px + 8px gap; table title ~40px, header ~36px, "sm" row ~36px.
    return max(3, math.ceil((rows * 36 + 80) / 38))

def stat(title, expr, x, y, w, thresholds=None, desc="", no_value="–", mappings=None):
    d = {"unit": "none", "noValue": no_value, "color": {"mode": "thresholds"},
         "thresholds": thresholds or {"mode": "absolute", "steps": [{"color": "text", "value": None}]}}
    if mappings: d["mappings"] = mappings
    return {"type": "stat", "title": title, "description": desc, "datasource": DS, "id": nid(),
        "gridPos": {"h": 2, "w": w, "x": x, "y": y},
        "targets": [tgt(expr, "A", instant=True)],
        "fieldConfig": {"defaults": d, "overrides": []},
        "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
            "colorMode": "value", "graphMode": "none", "textMode": "value", "justifyMode": "center",
            "text": {"valueSize": 24}}}

def row(title, y):
    return {"type": "row", "title": title, "id": nid(), "collapsed": False, "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "panels": []}

def count_or_zero(expr):
    # 0 only while the exporter reports accounts; no data when it is down.
    return f'count({expr}) or (0 * count(llm_quota_account_info{{{SEL}}}))'

panels = []
y = 0
WORST = f'max by ({BY}) (llm_quota_used_ratio{{{SEL}}})'
RED_FROM_1 = {"mode": "absolute", "steps": [{"color": "text", "value": None}, {"color": "red", "value": 1}]}
panels += [
    stat("Accounts", f'count(llm_quota_account_info{{{SEL}}})', 0, y, 4, desc="sub2api accounts with a supported quota or balance."),
    stat("≥ 90% used", count_or_zero(f'{WORST} >= 0.9'), 4, y, 4, thresholds=RED_FROM_1,
         desc="Accounts with at least one window at 90% or more (red bars); at 100% they are throttled until it resets."),
    stat("70–90% used", count_or_zero(f'{WORST} >= 0.7 < 0.9'), 8, y, 4,
         thresholds={"mode": "absolute", "steps": [{"color": "text", "value": None}, {"color": "yellow", "value": 1}]},
         desc="Accounts whose most-used window is between 70% and 90% (yellow bars)."),
    stat("Fetch failing", f'sum((llm_quota_up{{{SEL}}} == bool 0) * (1 - llm_quota_key_blacklisted{{{SEL}}}) * llm_quota_endpoint_supported{{{SEL}}})', 12, y, 4,
         thresholds=RED_FROM_1, desc="Accounts whose last quota fetch failed, excluding rejected keys and hosts without a balance API."),
    stat("Keys rejected", f'sum(llm_quota_key_blacklisted{{{SEL}}})', 16, y, 4,
         thresholds={"mode": "absolute", "steps": [{"color": "text", "value": None}, {"color": "orange", "value": 1}]},
         desc="Keys rejected with 401; skipped for 6h or until the key changes in sub2api."),
    stat("sub2api DB", f'max(llm_exporter_db_up{{{SEL}}})', 20, y, 4, no_value="✕ Down",
         thresholds={"mode": "absolute", "steps": [{"color": "red", "value": None}, {"color": "green", "value": 1}]},
         mappings=[{"type": "value", "options": {"0": {"text": "✕ DB error", "index": 0}, "1": {"text": "✓ OK", "index": 1}}}],
         desc="Whether the exporter could read accounts from the sub2api database on its last reload."),
]
y += 2

def provider_table(pid_, title, windows, currencies, rows, y, x=0, w=24):
    ps = f'provider="{pid_}", {SEL}'
    targets = [tq(f'max by ({BY}) (llm_quota_account_info{{{ps}}})', "K"),
               tq(f'max by ({BY}, plan) (llm_quota_plan_info{{{ps}}})', "J")]
    order = ["account_name", "plan"]
    rename = {"account_name": "Account", "plan": "Plan"}
    overrides = []
    for i, win in enumerate(windows):
        u, r = f"U{i}", f"R{i}"
        targets += [tq(f'max by ({BY}) (llm_quota_used_ratio{{window="{win}", {ps}}})', u),
                    tq(f'max by ({BY}) (llm_quota_reset_timestamp_seconds{{window="{win}", {ps}}}) - time()', r)]
        order += [f"Value #{u}", f"Value #{r}"]
        rename[f"Value #{u}"] = f"{WINDOW_TITLE[win]} used"
        rename[f"Value #{r}"] = f"{WINDOW_TITLE[win]} reset"
    for c in currencies:
        ref = f"B{c}"
        targets.append(tq(f'max by ({BY}) (llm_quota_balance{{kind="total", currency="{c}", {ps}}})', ref))
        order.append(f"Value #{ref}")
        rename[f"Value #{ref}"] = f"Balance {c}"
        overrides.append({"matcher": {"id": "byName", "options": f"Balance {c}"}, "properties": [
            {"id": "unit", "value": "currency" + c}, {"id": "decimals", "value": 2}, {"id": "custom.width", "value": 100}]})
    if pid_ == "deepseek":
        targets.append(tq(f'max by ({BY}) (llm_quota_balance_available{{{ps}}})', "BA"))
        order.append("Value #BA")
        rename["Value #BA"] = "Usable"
        overrides.append({"matcher": {"id": "byName", "options": "Usable"}, "properties": [
            {"id": "mappings", "value": [{"type": "value", "options": {
                "1": {"text": "✓ Yes", "color": "green", "index": 0}, "0": {"text": "✕ No", "color": "red", "index": 1}}}]},
            {"id": "custom.cellOptions", "value": {"type": "color-text"}}, {"id": "color", "value": {"mode": "fixed", "fixedColor": "text"}},
            {"id": "custom.width", "value": 70}]})
    targets.append(tq(STATE.replace(SEL, ps), "S"))
    order.append("Value #S")
    rename["Value #S"] = "Status"
    index = {name: i for i, name in enumerate(order)}
    exclude = {"Time": True, "provider": True, "account_id": True, "Value #K": True, "Value #J": True}
    overrides += [
        {"matcher": {"id": "byRegexp", "options": "/ used$/"}, "properties": [
            {"id": "unit", "value": "percentunit"}, {"id": "decimals", "value": 0}, {"id": "min", "value": 0}, {"id": "max", "value": 1},
            {"id": "thresholds", "value": THRESH}, {"id": "color", "value": {"mode": "thresholds"}},
            {"id": "custom.cellOptions", "value": {"type": "gauge", "mode": "basic", "valueDisplayMode": "color"}},
            # A window the account does not have: no bar, no green.
            {"id": "mappings", "value": [{"type": "special", "options": {"match": "null", "result": {"text": "n/a", "color": "transparent", "index": 0}}}]},
            {"id": "custom.minWidth", "value": 120 if w == 24 else 60}]},
        {"matcher": {"id": "byRegexp", "options": "/ reset$/"}, "properties": [
            {"id": "unit", "value": "dtdurations"}, {"id": "custom.width", "value": 125 if w == 24 else 82},
            {"id": "mappings", "value": [{"type": "range", "options": {"from": -1e12, "to": 0, "result": {"text": "stale", "color": "orange", "index": 0}}}]},
            {"id": "custom.cellOptions", "value": {"type": "color-text"}}, {"id": "color", "value": {"mode": "fixed", "fixedColor": "text"}}]},
        {"matcher": {"id": "byName", "options": "Account"}, "properties": [{"id": "custom.width", "value": 160 if w == 24 else 130}]},
        {"matcher": {"id": "byName", "options": "Plan"}, "properties": [{"id": "custom.width", "value": 90}]},
        {"matcher": {"id": "byName", "options": "Status"}, "properties": [
            {"id": "mappings", "value": STATE_MAP}, {"id": "custom.cellOptions", "value": {"type": "color-text"}},
            {"id": "color", "value": {"mode": "fixed", "fixedColor": "text"}}, {"id": "custom.width", "value": 140 if w == 24 else 95}]},
    ]
    what = ", ".join(WINDOW_TITLE[win] for win in windows) or "balance"
    return {"type": "table", "title": f"{title} · {rows} account{'s' if rows != 1 else ''}", "datasource": DS, "id": nid(),
        "description": f"{title} accounts ({what}). Bars show the share of each window used: green below 70%, yellow below 90%, red above. Reset is the time until that window resets.",
        "gridPos": {"h": table_height(rows), "w": w, "x": x, "y": y},
        "targets": targets,
        "transformations": [
            {"id": "merge", "options": {}},
            {"id": "organize", "options": {"excludeByName": exclude, "indexByName": index, "renameByName": rename}},
        ],
        "fieldConfig": {"defaults": {"custom": {"align": "auto", "cellOptions": {"type": "auto"}}, "noValue": "–"},
            "overrides": overrides},
        "options": {"showHeader": True, "cellHeight": "sm", "sortBy": [{"displayName": "Account", "desc": False}]}}

# Providers with many accounts get a full-width table each; small ones
# (<= 3 accounts) are paired side by side to keep the first screen short.
present_providers = [p for p in PROVIDERS if COUNTS.get(p[0], 0) > 0]
large = [p for p in present_providers if COUNTS[p[0]] > 3]
small = [p for p in present_providers if COUNTS[p[0]] <= 3]
for pid_, title, windows, currencies in large:
    t = provider_table(pid_, title, windows, currencies, COUNTS[pid_], y)
    panels.append(t)
    y += t["gridPos"]["h"]
for i in range(0, len(small), 2):
    pair = small[i:i + 2]
    w = 24 // len(pair)
    tables = [provider_table(p[0], p[1], p[2], p[3], COUNTS[p[0]], y, x=j * w, w=w) for j, p in enumerate(pair)]
    h = max(t["gridPos"]["h"] for t in tables)
    for t in tables:
        t["gridPos"]["h"] = h
    panels += tables
    y += h

# ---- usage from sub2api usage_logs ----
USAGE_WINDOWS = [int(d) for d in args.usage_windows.split(",") if d.strip()]
# One fixed colour per provider so that a provider looks the same in every chart.
PROVIDER_COLORS = {"zhipu": "blue", "kimi": "purple", "minimax": "orange", "opencode_go": "green",
    "command_code": "yellow", "cline": "red", "deepseek": "dark-blue", "anthropic": "#d97757",
    "openai": "#10a37f", "gemini": "#4285f4", "antigravity": "#7c4dff"}
COLOR_OVERRIDES = [{"matcher": {"id": "byName", "options": p}, "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": c}}]}
    for p, c in PROVIDER_COLORS.items()]
USAGE_DESC = ("From sub2api usage_logs: every account that served requests, grouped by provider "
    "(the quota provider for tracked accounts, otherwise the sub2api platform). "
    "Tokens include input, output, cache writes and cache reads. Value is the list price of those tokens "
    "(usage_logs.total_cost, USD) before any group rate multiplier.")

def usage_bars(title, metric, extra, granularity, unit, x, y):
    return {"type": "barchart", "title": title, "datasource": DS, "id": nid(),
        "description": USAGE_DESC + " Hours and days start in the exporter's usage.timezone.",
        "gridPos": {"h": 9, "w": 12, "x": x, "y": y},
        "targets": [tq(f'sum by (provider, bucket) ({metric}{{granularity="{granularity}"{extra}, {SEL}}})', "A")],
        "transformations": [
            {"id": "groupingToMatrix", "options": {"columnField": "provider", "rowField": "bucket", "valueField": "Value", "emptyValue": "zero"}},
            {"id": "sortBy", "options": {"sort": [{"field": "bucket\\provider"}]}},
            {"id": "organize", "options": {"renameByName": {"bucket\\provider": "Start"}}},
        ],
        "fieldConfig": {"defaults": {"unit": unit, "min": 0, "color": {"mode": "palette-classic-by-name"},
            "custom": {"fillOpacity": 85, "lineWidth": 0, "gradientMode": "none"}}, "overrides": COLOR_OVERRIDES},
        "options": {"stacking": "normal", "orientation": "vertical", "xField": "Start", "showValue": "never",
            "barWidth": 0.85, "groupWidth": 0.8, "xTickLabelRotation": -45 if granularity == "hour" else 0, "xTickLabelSpacing": 0,
            "legend": {"showLegend": True, "displayMode": "list", "placement": "bottom"},
            "tooltip": {"mode": "multi", "sort": "desc"}}}

def usage_table(title, by, y, h, desc):
    targets, order, rename, overrides = [], list(by), {k: k.replace("_", " ").capitalize() for k in by}, list()
    rename.update({"provider": "Provider", "account_name": "Account"})
    for d in USAGE_WINDOWS:
        win = f'window="{d}d", {SEL}'
        t, v, r = f"T{d}", f"V{d}", f"Q{d}"
        targets += [tq(f'sum by ({", ".join(by)}) (llm_usage_requests{{{win}}})', r),
                    tq(f'sum by ({", ".join(by)}) (llm_usage_tokens{{{win}}})', t),
                    tq(f'sum by ({", ".join(by)}) (llm_usage_cost_usd{{kind="total", {win}}})', v)]
        order += [f"Value #{r}", f"Value #{t}", f"Value #{v}"]
        rename.update({f"Value #{r}": f"Requests {d}d", f"Value #{t}": f"Tokens {d}d", f"Value #{v}": f"Value {d}d"})
    overrides += [
        {"matcher": {"id": "byRegexp", "options": "/^Requests /"}, "properties": [{"id": "unit", "value": "short"}, {"id": "decimals", "value": 1}]},
        {"matcher": {"id": "byRegexp", "options": "/^Tokens /"}, "properties": [{"id": "unit", "value": "short"}, {"id": "decimals", "value": 2}]},
        {"matcher": {"id": "byRegexp", "options": "/^Value /"}, "properties": [{"id": "unit", "value": "currencyUSD"}, {"id": "decimals", "value": 2}]},
    ]
    exclude = {"Time": True, "account_id": True}
    return {"type": "table", "title": title, "datasource": DS, "id": nid(), "description": desc,
        "gridPos": {"h": h, "w": 24, "x": 0, "y": y}, "targets": targets,
        "transformations": [
            {"id": "merge", "options": {}},
            {"id": "organize", "options": {"excludeByName": exclude, "indexByName": {n: i for i, n in enumerate(order)}, "renameByName": rename}},
            {"id": "sortBy", "options": {"sort": [{"field": f"Value {USAGE_WINDOWS[-1]}d", "desc": True}]}},
        ],
        "fieldConfig": {"defaults": {"custom": {"align": "auto", "cellOptions": {"type": "auto"}}, "noValue": "0"}, "overrides": overrides},
        "options": {"showHeader": True, "cellHeight": "sm"}}

if USAGE_WINDOWS:
    panels.append(row("Usage (sub2api usage_logs)", y)); y += 1
    t = usage_table("Usage by provider", ["provider"], y, 7,
        USAGE_DESC + " Windows are trailing, to the hour: 1d is this hour plus the 23 before it.")
    panels.append(t); y += t["gridPos"]["h"]
    panels += [
        usage_bars("Tokens · last 24 hours", "llm_usage_bucket_tokens", "", "hour", "short", 0, y),
        usage_bars("Tokens · last 7 days", "llm_usage_bucket_tokens", "", "day", "short", 12, y),
    ]; y += 9
    panels += [
        usage_bars("Value (USD) · last 24 hours", "llm_usage_bucket_cost_usd", ', kind="total"', "hour", "currencyUSD", 0, y),
        usage_bars("Value (USD) · last 7 days", "llm_usage_bucket_cost_usd", ', kind="total"', "day", "currencyUSD", 12, y),
    ]; y += 9

# ---- below the fold ----
LEGEND = "{{provider}} / {{account_name}}"
SERIES = {"lineWidth": 2, "fillOpacity": 0, "showPoints": "never", "spanNulls": 600000}
N = max(1, sum(COUNTS.values()))

def ratio_ts(title, window, x, y, w, h, time_from=None, no_value="No data"):
    p = {"type": "timeseries", "title": title, "datasource": DS, "id": nid(),
        "description": f"Share of the {window} window used, per account. Dashed lines mark 70% and 90%.",
        "gridPos": {"h": h, "w": w, "x": x, "y": y},
        "targets": [tgt(f'max by ({BY}) (llm_quota_used_ratio{{window="{window}", {SEL}}})', "A", LEGEND)],
        "fieldConfig": {"defaults": {"unit": "percentunit", "min": 0, "decimals": 0, "noValue": no_value,
            "color": {"mode": "palette-classic-by-name"}, "thresholds": THRESH,
            "custom": dict(SERIES, thresholdsStyle={"mode": "dashed"}, axisSoftMax=1)}, "overrides": []},
        "options": {"legend": {"displayMode": "table", "placement": "right" if w == 24 else "bottom", "showLegend": True, "calcs": ["lastNotNull", "max"], "sortBy": "Name", "sortDesc": False},
            "tooltip": {"mode": "multi", "sort": "desc"}}}
    if time_from: p["timeFrom"] = time_from
    return p

chart_h = max(8, min(14, math.ceil((N * 24 + 70) / 38)))
# History charts only for windows some provider here has.
present = {w for p in PROVIDERS if COUNTS.get(p[0], 0) > 0 for w in p[2]}
panels.append(row("History", y)); y += 1
if "5h" in present:
    panels.append(ratio_ts("5h window used (last 2 days)", "5h", 0, y, 24, chart_h, time_from="2d")); y += chart_h
long_windows = [w for w in ("weekly", "monthly") if w in present]
for i, w in enumerate(long_windows):
    panels.append(ratio_ts(f"{WINDOW_TITLE[w]} window used", w, i * (24 // len(long_windows)), y, 24 // len(long_windows), chart_h))
if long_windows:
    y += chart_h

def balance_ts(currency, x, who):
    return {"type": "timeseries", "title": f"Balance ({currency})", "datasource": DS, "id": nid(),
        "description": f"Total balance in {currency}; reported by {who}.",
        "gridPos": {"h": 7, "w": 12, "x": x, "y": y},
        "targets": [tgt(f'max by ({BY}) (llm_quota_balance{{kind="total", currency="{currency}", {SEL}}})', "A", LEGEND)],
        "fieldConfig": {"defaults": {"unit": "currency" + currency, "min": 0, "noValue": f"No account here reports a {currency} balance",
            "color": {"mode": "palette-classic-by-name"}, "custom": SERIES}, "overrides": []},
        "options": {"legend": {"displayMode": "table", "placement": "right", "showLegend": True, "calcs": ["lastNotNull", "min"]},
            "tooltip": {"mode": "multi", "sort": "desc"}}}

# Balance charts only when some provider here reports a balance.
currencies = sorted({c for p in PROVIDERS if COUNTS.get(p[0], 0) > 0 for c in p[3]})
if currencies:
    who = {"CNY": "DeepSeek", "USD": "DeepSeek, Command Code and Cline"}
    w = 24 // len(currencies)
    for i, c in enumerate(currencies):
        b = balance_ts(c, i * w, who[c]); b["gridPos"]["w"] = w
        panels.append(b)
    y += 7

panels.append(row("Details", y)); y += 1
details = {"type": "table", "title": "Accounts", "datasource": DS, "id": nid(),
    "description": "Fetch details per account. Data age is how old the numbers are; HTTP is the last upstream status (0 = no response).",
    "gridPos": {"h": table_height(N), "w": 24, "x": 0, "y": y},
    "targets": [
        tq(f'max by ({BY}, platform, status, schedulable) (llm_quota_account_info{{{SEL}}})', "K"),
        tq(f'max by ({BY}, plan) (llm_quota_plan_info{{{SEL}}})', "J"),
        tq(STATE, "S"),
        tq(f'time() - max by ({BY}) (llm_quota_last_success_timestamp_seconds{{{SEL}}})', "H"),
        tq(f'max by ({BY}) (llm_quota_last_http_status{{{SEL}}})', "L"),
    ],
    "transformations": [
        {"id": "merge", "options": {}},
        {"id": "organize", "options": {
            "excludeByName": {"Time": True, "Value #K": True, "Value #J": True},
            "indexByName": {"provider": 0, "account_name": 1, "account_id": 2, "plan": 3, "Value #S": 4, "Value #H": 5,
                "Value #L": 6, "platform": 7, "status": 8, "schedulable": 9},
            "renameByName": {"provider": "Provider", "account_name": "Account", "account_id": "ID", "plan": "Plan",
                "Value #S": "Status", "Value #H": "Data age", "Value #L": "HTTP",
                "platform": "sub2api platform", "status": "sub2api status", "schedulable": "Schedulable"}}},
        {"id": "sortBy", "options": {"sort": [{"field": "Account"}]}},
        {"id": "sortBy", "options": {"sort": [{"field": "Provider"}]}},
    ],
    "fieldConfig": {"defaults": {"custom": {"align": "auto", "cellOptions": {"type": "auto"}}, "noValue": "–"},
        "overrides": [
            {"matcher": {"id": "byName", "options": "Status"}, "properties": [
                {"id": "mappings", "value": STATE_MAP}, {"id": "custom.cellOptions", "value": {"type": "color-text"}}]},
            {"matcher": {"id": "byName", "options": "Data age"}, "properties": [
                {"id": "unit", "value": "s"}, {"id": "decimals", "value": 0},
                {"id": "thresholds", "value": {"mode": "absolute", "steps": [{"color": "text", "value": None}, {"color": "orange", "value": 900}, {"color": "red", "value": 1800}]}},
                {"id": "color", "value": {"mode": "thresholds"}}, {"id": "custom.cellOptions", "value": {"type": "color-text"}}]},
            {"matcher": {"id": "byName", "options": "ID"}, "properties": [{"id": "custom.width", "value": 70}]},
        ]},
    "options": {"showHeader": True, "cellHeight": "sm"}}
panels.append(details); y += details["gridPos"]["h"]

if USAGE_WINDOWS:
    t = usage_table("Usage by account", ["provider", "account_name", "account_id"], y, 12,
        USAGE_DESC + " Accounts with no requests in the longest window still show, with zeros.")
    panels.append(t); y += t["gridPos"]["h"]

status_h = max(5, min(14, math.ceil((N * 22 + 80) / 38)))
panels.append({"type": "state-timeline", "title": "Fetch status", "datasource": DS, "id": nid(),
    "description": "Per-account fetch result over time. No API: the account's host (a relay) has no balance endpoint.",
    "gridPos": {"h": status_h, "w": 24, "x": 0, "y": y},
    "targets": [tgt(STATE, "A", LEGEND)],
    "fieldConfig": {"defaults": {"color": {"mode": "fixed", "fixedColor": "text"}, "mappings": STATE_MAP,
        "custom": {"fillOpacity": 80, "lineWidth": 0, "spanNulls": 600000}}, "overrides": []},
    "options": {"showValue": "never", "mergeValues": True, "rowHeight": 0.8,
        "legend": {"showLegend": True, "displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "single"}}})
y += status_h

dash = {"uid": args.uid, "title": args.title, "tags": ["llm", "sub2api", "quota"],
    "timezone": "browser", "schemaVersion": 41, "version": 11, "editable": True, "graphTooltip": 1,
    "refresh": args.refresh, "time": {"from": "now-7d", "to": "now"},
    "templating": {"list": []},
    "annotations": {"list": []}, "links": [], "panels": panels}
with open(args.out, "w") as f:
    json.dump(dash, f, indent=2, ensure_ascii=False)
    f.write("\n")
