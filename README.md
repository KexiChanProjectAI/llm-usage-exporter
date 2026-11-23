# llm-usage-exporter

从 sub2api 数据库读取 Coding Plan / 订阅类账号，定时查询各提供商的用量窗口，以 Prometheus 指标暴露。

支持：GLM（智谱，`zhipu`）、MiniMax、Kimi、OpenCode Go、Command Code、Cline 的用量窗口，以及 DeepSeek 按量付费账号的余额。
另外从 sub2api 的 `usage_logs` 统计每个账号、每个提供商的 Token 用量和按标价折算的等价价值。

## 工作方式

- 每 `database.reload_interval` 从 sub2api 的 `accounts` / `proxies` 表读取未删除的 `apikey` / `upstream` 账号。
  OAuth 账号不会被读取。
- 账号归属哪家提供商，沿用 sub2api 的判定（`GetCodingPlanProvider` 与 `IsOpenCodeGoUsageAccount`），只认官方域名，指向中转站的 Key 不会被发往厂商额度接口。
- 后台按 `providers.<id>.interval` 定时查询，结果缓存在内存；Prometheus 抓取 `/metrics` 只读缓存，不会触发上游请求。
- 同一 Key、同一端点、同一代理的多个账号共用一次查询。
- 查询失败保留上次的值，超过 `collect.max_staleness` 后不再导出。
- 返回 `auth_blacklist.status_codes`（默认 401）的 Key 会被跳过 `auth_blacklist.duration`（默认 6 小时）；sub2api 里换了 Key 之后立即重新查询。
  黑名单在内存中，重启后会再试一次。
- 不跟随重定向，不读取 `HTTP(S)_PROXY` 环境变量，代理只由配置决定。

## 配置文件中的账号

不在 sub2api 里的账号（例如直接用官方 Key 的 DeepSeek 账号）可以写在配置文件的 `accounts` 里，和 sub2api 的账号一起采集：

```yaml
accounts:
  - name: DeepSeek Cyber
    provider: deepseek
    api_key: ${DEEPSEEK_CYBER_KEY}
```

`base_url` 默认是该提供商的官方地址；`account_mode` 对 zhipu、kimi、minimax 默认为 `coding`；`proxy` 的优先级与 sub2api 账号的代理相同。
这些账号的 `account_id` 标签为 `cfg:<name>`，`status` 为 `config`。
读取 sub2api 失败时，这些账号也随之保持上一次加载的状态。

## Token 用量与等价价值

从 sub2api 的 `usage_logs` 读取，不请求上游；配置在 `usage` 下（见 `config.example.yaml`）。

- 统计所有有请求记录的账号（含 OAuth 账号和已软删除的账号）。`provider` 标签对本 exporter 跟踪额度的账号是它的提供商，其余账号是 sub2api 的 `platform`（如 `anthropic`、`openai`）。
- 价格沿用 sub2api 记账时算出的费用（美元），不另读价格表：
  - `kind="total"`：等价价值，即 Token 按模型标价计算的费用（`total_cost`，未乘分组倍率）；
  - `kind="actual"`：sub2api 向用户实际扣费（`actual_cost`，已乘分组倍率）；
  - `kind="account"`：账号侧成本，与 sub2api 看板的“账号成本”算法相同：`COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)`。
- 内存里按“账号 × 小时”缓存。启动时和每 `resync_interval` 读一次完整区间；其间每 `interval` 只重读最近 `lookback`（向下取整到整点）。
- 窗口按小时对齐：`1d` 是当前小时加之前 23 个小时。小时和天的起点按 `usage.timezone`。
- 读失败时保留上次的值，`llm_usage_up` 为 0。

只读角色需要额外授权：

```sql
GRANT SELECT ON usage_logs TO llm_usage_exporter;
```

## 代理

优先级从高到低：

1. `providers.<id>.proxy`
2. 账号在 sub2api 里的代理（`use_account_proxy: true` 时，默认开启）
3. 全局 `proxy`
4. 直连

任一层写 `direct` 都会强制直连，不再往下找。
支持 `http`、`https`、`socks5`、`socks5h`。

## 各提供商的查询接口

| provider | 接口 | 窗口 |
|---|---|---|
| `zhipu` | `{open.bigmodel.cn\|api.z.ai}/api/monitor/usage/quota/limit` | 5h、weekly |
| `minimax` | `{api.minimax.io\|api.minimaxi.com}/v1/api/openplatform/coding_plan/remains` | 5h、weekly |
| `kimi` | `api.kimi.com/coding/v1/usages` | 5h、weekly 或 monthly |
| `opencode_go` | `opencode.ai/zen/go/v1/usage` | 5h、weekly、monthly |
| `command_code` | `api.commandcode.ai/alpha/*`（4 次请求） | 5h、weekly、monthly，余额 |
| `cline` | `api.cline.bot/api/v1/users/*` | 5h、weekly、monthly，余额 |
| `deepseek` | `api.deepseek.com/user/balance` | 只有余额（CNY / USD） |

DeepSeek 与 sub2api 一致，请求 `<OpenAI 格式 base_url>/user/balance`，即账号推理时本来就在用的主机，因此中转站只要实现了这个接口也能查。
没有这个接口的中转站（返回 400/404/405/501，或包着 "not found" 的错误，或 2xx 但没有 `balance_infos`）不算失败：`llm_quota_endpoint_supported` 为 0，每 `collect.unsupported_retry`（默认 6 小时）才再试一次。
Anthropic 协议的账号只在 base_url 是官方 `api.deepseek.com` 时查询：sub2api 对这类账号会回退到官方地址，那样会把中转站的 Key 发给 DeepSeek。

## 指标

公共标签：`provider`、`account_id`、`account_name`；配置了 `instance` 时所有指标另有 `sub2api_instance`。

| 指标 | 额外标签 | 说明 |
|---|---|---|
| `llm_quota_used_ratio` | `window` | 已用比例，0–1，可能超过 1 |
| `llm_quota_reset_timestamp_seconds` | `window` | 窗口重置时间 |
| `llm_quota_window_used` / `llm_quota_window_limit` | `window`, `unit` | 绝对用量（Command Code 为 `usd`，Kimi 老套餐为 `count`） |
| `llm_quota_balance` | `kind`, `currency` | 余额（Command Code、Cline、DeepSeek）；`kind` 为 `total`、`monthly`、`purchased`、`free`、`granted`、`topped_up` |
| `llm_quota_balance_available` | | DeepSeek 的 `is_available`：余额是否可用 |
| `llm_quota_plan_info` | `plan` | 套餐名 |
| `llm_quota_up` | | 上次查询是否成功 |
| `llm_quota_auth_valid` | | 上次查询是否被 401/403 拒绝 |
| `llm_quota_endpoint_supported` | | 账号所在主机是否提供该额度/余额接口（中转站常为 0） |
| `llm_quota_key_blacklisted` | | Key 是否在黑名单中 |
| `llm_quota_key_blacklist_until_timestamp_seconds` | | 黑名单到期时间 |
| `llm_quota_last_attempt_timestamp_seconds` | | 上次查询时间 |
| `llm_quota_last_success_timestamp_seconds` | | 上次成功时间 |
| `llm_quota_fetch_duration_seconds` | | 上次查询耗时 |
| `llm_quota_last_http_status` | | 上次 HTTP 状态码，0 表示没有响应 |
| `llm_quota_account_info` | `platform`, `status`, `schedulable` | sub2api 账号信息 |
| `llm_quota_fetch_total` | `result` | 按结果累计的查询次数；没有 account 标签 |
| `llm_exporter_accounts` | | 各提供商账号数；没有 account 标签 |
| `llm_exporter_db_up` | | 上次读库是否成功 |
| `llm_exporter_db_last_success_timestamp_seconds` | | 上次读库成功时间 |

用量指标（来自 `usage_logs`）：

| 指标 | 标签 | 说明 |
|---|---|---|
| `llm_usage_requests` | 公共标签, `window` | 窗口内请求数；`window` 为 `usage.windows` 中的天数，如 `7d` |
| `llm_usage_tokens` | 公共标签, `window`, `type` | 窗口内 Token 数；`type` 为 `input`、`output`、`cache_creation`、`cache_read` |
| `llm_usage_cost_usd` | 公共标签, `window`, `kind` | 窗口内费用（美元）；`kind` 为 `total`（等价价值）、`actual`、`account` |
| `llm_usage_bucket_requests` / `llm_usage_bucket_tokens` / `llm_usage_bucket_cost_usd` | `provider`, `granularity`, `bucket`（另有 `type` / `kind`） | 按提供商汇总的每小时（最近 `hourly_buckets` 个）/每天（最近 `daily_buckets` 天）用量；`bucket` 为起点，如 `2026-11-23 14:00`、`2026-11-23`；有用量的提供商补零 |
| `llm_usage_up` | | 上次读 `usage_logs` 是否成功 |
| `llm_usage_last_success_timestamp_seconds` | | 上次成功时间 |
| `llm_usage_query_duration_seconds` | | 上次读取耗时 |

桶指标的数值直接来自数据库，不依赖 Prometheus 的历史，所以刚部署就能画出完整的 24 小时和 7 天柱状图。

示例告警：

```promql
llm_quota_used_ratio{window="5h"} > 0.9
llm_quota_up == 0 and llm_quota_key_blacklisted == 0
llm_quota_key_blacklisted == 1
```

## 运行

```sh
cp config.example.yaml config.yaml
SUB2API_DB_PASSWORD=... ./llm-usage-exporter -config config.yaml
```

`/healthz` 在最近一次读库失败时返回 503。

只读数据库角色：

```sql
CREATE ROLE llm_usage_exporter LOGIN PASSWORD '...';
GRANT CONNECT ON DATABASE sub2api TO llm_usage_exporter;
GRANT USAGE ON SCHEMA public TO llm_usage_exporter;
GRANT SELECT ON accounts, proxies, usage_logs TO llm_usage_exporter;
ALTER ROLE llm_usage_exporter SET default_transaction_read_only = on;
```

## 部署示例

`deploy/` 下是一套 systemd + Prometheus + Grafana 的部署示例：

- `deploy/systemd/llm-usage-exporter.service`：以专用用户运行，数据库密码放在 `EnvironmentFile`（`SUB2API_DB_PASSWORD=...`，权限 0640）。
- `deploy/prometheus/scrape-llm-usage.yaml`：追加到 `prometheus.yml` 的 `scrape_configs`。
- `deploy/grafana/llm-usage-dashboard.json`：Grafana 看板，由 `gen_dashboard.py` 生成。
- `deploy/grafana/provisioning-llm-usage.yaml`：Grafana 文件方式加载看板的配置。
- `deploy/nginx/redirect-to-public-dashboard.conf.example`：用一个短域名 302 到公开看板。

## Grafana 看板

看板没有模板变量，因此可以直接设为 Grafana 公开看板（public dashboard 不支持模板变量）。
包含：总览状态块、当前额度表、Token 用量与等价价值（按提供商的窗口汇总表，最近 24 小时和 7 天的堆叠柱状图，每个提供商一种颜色，以及按账号的明细表）、5h / 周 / 月用量曲线、查询状态时间线和余额曲线。
`--usage-windows` 要与 exporter 的 `usage.windows` 一致（默认 `1,7,30`），传空字符串则不画用量部分。

数据源 uid、标题和 Prometheus job 名可以在生成时指定：

```sh
python3 deploy/grafana/gen_dashboard.py out.json --datasource-uid <your-prometheus-uid> --title "LLM quota" --job llm_usage
```

同一个 Prometheus 采集多个 sub2api 时，每个 exporter 的配置里设置不同的 `instance`，它会作为 `sub2api_instance` 标签加到所有指标上；再用 `--instance <name>` 为每个实例生成一个看板（`--uid` 也要不同）。
首屏是状态条加上每个提供商一张表，表的高度按账号数确定，所以所有账号不用滚动就能看到；历史和明细在下面。
账号数来自 `--prometheus <url>`（从运行中的 Prometheus 统计）或 `--providers zhipu=8,kimi=4`；账号增减后重新生成即可。
`deploy/systemd/llm-usage-exporter@.service` 是多实例用的模板单元，配置文件为 `/etc/llm-usage-exporter/<instance>.yaml`。

仓库里的 `llm-usage-dashboard.json` 使用默认值（数据源 uid `prometheus`）。

## 构建与测试

```sh
go test ./...
go build -o bin/llm-usage-exporter ./cmd/llm-usage-exporter
docker build -t llm-usage-exporter .
```

`internal/source` 的集成测试需要一个已执行 sub2api migrations 的库，设置 `LLM_EXPORTER_TEST_DSN` 后运行；写入都在事务里并回滚。

## 许可

GNU LGPL v3.0，见 `LICENSE`（LGPL 是对 `COPYING` 中 GPL v3 的补充条款）。

`internal/provider` 中的解析与查询逻辑移植自 [sub2api](https://github.com/Wei-Shaw/sub2api)（commit `328db26`，LGPL v3.0）。
