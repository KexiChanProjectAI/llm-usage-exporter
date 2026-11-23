# llm-usage-exporter

从 sub2api 数据库读取 Coding Plan / 订阅类账号，定时查询各提供商的用量窗口，以 Prometheus 指标暴露。

支持：GLM（智谱，`zhipu`）、MiniMax、Kimi、OpenCode Go、Command Code、Cline。

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

## 指标

公共标签：`provider`、`account_id`、`account_name`。

| 指标 | 额外标签 | 说明 |
|---|---|---|
| `llm_quota_used_ratio` | `window` | 已用比例，0–1，可能超过 1 |
| `llm_quota_reset_timestamp_seconds` | `window` | 窗口重置时间 |
| `llm_quota_window_used` / `llm_quota_window_limit` | `window`, `unit` | 绝对用量（Command Code 为 `usd`，Kimi 老套餐为 `count`） |
| `llm_quota_balance` | `kind`, `currency` | 余额（Command Code、Cline） |
| `llm_quota_plan_info` | `plan` | 套餐名 |
| `llm_quota_up` | | 上次查询是否成功 |
| `llm_quota_auth_valid` | | 上次查询是否被 401/403 拒绝 |
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
GRANT SELECT ON accounts, proxies TO llm_usage_exporter;
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
包含：总览状态块、当前额度表、5h / 周 / 月用量曲线、查询状态时间线和余额曲线。

数据源 uid、标题和 Prometheus job 名可以在生成时指定：

```sh
python3 deploy/grafana/gen_dashboard.py out.json --datasource-uid <your-prometheus-uid> --title "LLM quota" --job llm_usage
```

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
