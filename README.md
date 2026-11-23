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
- 内存里按“账号 × 小时”缓存。sub2api 在记录请求时才写入 `created_at`，过去的小时不会再变，所以只在启动时读一次完整区间，其后每 `interval` 只重读最近 `lookback`（向下取整到整点）。
  每 `resync_interval`（默认 24 小时）再读一次完整区间，只为去掉其间被删除的记录（用量清理、删除账号）。
- 数据库压力（实测 sub2api 主库，30 天约 83 万行）：增量查询约 3 ms；完整区间读取约 1 秒，走 `created_at` 索引，只在启动和每次 resync 时发生。Prometheus 抓取只读内存。
- 窗口按小时对齐：`1d` 是当前小时加之前 23 个小时。小时和天的起点按 `usage.timezone`。
- 读失败时保留上次的值，`llm_usage_up` 为 0。

只读角色需要额外授权：

```sql
GRANT SELECT ON usage_logs TO llm_usage_exporter;
```

## 额度估计

提供商只报告每个窗口用了百分之几，不报告窗口有多大。
把 sub2api 在窗口内实际记下的用量（按标价折算的美元）除以已用比例，就得到窗口容量的估计，进而得到剩余额度。

### exporter 做的部分

每次额度查询成功后，立即读一次 `usage_logs`，得到该 Key 从窗口起点到查询时刻的用量，导出为 `llm_quota_window_consumed`，与同一次查询的 `llm_quota_used_ratio` 配对。

- 窗口起点 = 重置时间 − 窗口长度（`5h` 为 5 小时，`weekly` 为 7 天）。
  无论窗口按固定时刻滚动（MiniMax：所有账号同一时刻重置），还是在上一个窗口结束后、第一次请求时才开始（GLM：空闲的 5h 窗口报 0% 且没有重置时间），重置于 R 的窗口都开始于 R 减窗口长度，所以不需要知道是哪一种。
- 没有重置时间（窗口还没开始）、或重置时间比查询时刻晚出一个以上的窗口长度（长度不符）时，不导出。
  窗口在查询前已经结束时，用量只算到重置时间。
- `monthly` 不估计：按自然月、30 天还是账单周期，各家不同。
- 同一 Key 的多个 sub2api 账号合计（它们共用一份额度）；配置文件里的账号在 `usage_logs` 里没有记录，不导出。
- 每次查询一条 SQL，走 `(account_id, created_at)` 索引；实测主库 7 天窗口约 2 ms。由 `usage.quota_windows` 控制（默认开启，需要 `usage.enabled`）。

### Prometheus / Grafana 做的部分

除法和跨窗口的平滑放在 Prometheus 的记录规则里（`deploy/prometheus/rules-llm-quota-estimate.yaml`，每 5 分钟计算一次），看板只读记录下的序列：

- 每次查询是一个样本：容量 ≈ 已记用量 ÷ 已用比例。
  `llm_quota:window_capacity_usd:estimate` 是账号在回看区间内的 `sum(用量) / sum(比例)`，相当于按比例加权，后期、精确的样本权重大；已用不到 10% 的样本丢弃（GLM 只报整数百分比）；结果为 0（本 sub2api 没有记录）不算估计。
  回看区间：5h 窗口 7 天，weekly 28 天。改这些参数只需改规则文件。
- 同一 Key 在本 sub2api 之外的使用（另一个 sub2api、个人、其他工具）会抬高已用比例，却没有 `usage_logs` 记录，只会把估计压低。
  同一套餐的账号容量相同，所以 `llm_quota:plan_capacity_usd:max` 取同一 `provider` + `plan` 下各账号估计的最大值；不报套餐名的提供商（如 Kimi）用账号自己的估计。
- 剩余 = 套餐容量 × (1 − 已用比例)，这与谁用掉的无关；已用 100% 的窗口在没有估计时也记为 0。
  “Usable now”取各窗口剩余的最小值，只在账号的每个 5h / weekly 窗口都有估计时显示；任一窗口（包括 monthly）用尽时为 0。
- “Via sub2api” = 账号自己的 weekly 估计 ÷ 套餐容量；明显低于 100% 说明 Key 在别处也在用。

局限：估计的单位是 Token 的标价，不是提供商自己的计量。模型组合变化、高峰时段倍率等都会让同样的标价对应不同的额度消耗，提供商统计滞后也会让刚发生的请求暂时被高估。
看板 History 里的“implied capacity · per-fetch samples”曲线是每次查询单独算出的容量，曲线平稳才说明估计可信。

### 为什么这样分工

| | 在 exporter 里 | 在 Prometheus 里 |
|---|---|---|
| 窗口起点各账号不同，且随首次使用变化 | 有重置时间和数据库，按秒精确求和 | PromQL 的区间长度只能是常量，按序列变化的起点做不到；用计数器差值近似要受抓取间隔、重启归零和递归记录规则的限制 |
| 与已用比例配对 | 在同一次查询后立即读，天然对齐 | 两个指标各自刷新，对不齐 |
| 跨窗口的历史 | 要自己持久化状态，重启即丢 | 已有 180 天保留，重启无影响 |
| 估计方法（最小比例、回看区间、套餐取最大值） | 改了要重新部署 | 只改规则文件，可随时调整、回溯 |

所以：需要数据库和重置时间的“窗口内用量”在 exporter 里算；除法、平滑和跨账号比较在 Prometheus 里算。
7 天 / 28 天的子查询放在记录规则里每 5 分钟算一次，而不是在看板里每次刷新都算（公开看板每个访客每 30 秒刷新一次）。

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
| `llm_quota_window_start_timestamp_seconds` | `window` | 当前窗口的起点：重置时间减窗口长度（只有 `5h`、`weekly`） |
| `llm_quota_window_consumed` | `window`, `unit` | 从窗口起点到这次额度查询，sub2api 为该 Key 记下的用量（`usage_logs`）；`unit` 为 `usd`（标价）、`tokens`、`requests`。见“额度估计” |
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
- `deploy/prometheus/rules-llm-quota-estimate.yaml`：额度估计的记录规则，加到 `prometheus.yml` 的 `rule_files`。
- `deploy/grafana/llm-usage-dashboard.json`：Grafana 看板，由 `gen_dashboard.py` 生成。
- `deploy/grafana/provisioning-llm-usage.yaml`：Grafana 文件方式加载看板的配置。
- `deploy/nginx/redirect-to-public-dashboard.conf.example`：用一个短域名 302 到公开看板。

## Grafana 看板

看板没有模板变量，因此可以直接设为 Grafana 公开看板（public dashboard 不支持模板变量）。
包含：总览状态块、当前额度表、额度估计（套餐容量和每个账号的估计剩余，见“额度估计”）、Token 用量与等价价值（按提供商的窗口汇总表，最近 24 小时和 7 天的堆叠柱状图，每个提供商一种颜色，以及按账号的明细表）、5h / 周 / 月用量曲线、查询状态时间线和余额曲线。
`--usage-windows` 要与 exporter 的 `usage.windows` 一致（默认 `1,7,30`），传空字符串则不画用量部分。
`--estimate-windows`（默认 `5h,weekly`）选择要估计的窗口，传空字符串则不画估计部分；需要 exporter 开启 `usage.quota_windows`，Prometheus 加载估计的记录规则。

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
