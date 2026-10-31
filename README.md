# jenkins-exporter

src-openeuler 门禁 Jenkins 的只读指标 exporter（Prometheus `/metrics`）。

后台每 60s 通过 Jenkins REST API（HTTP Basic + 文件 token、白名单 job、串行、单请求 10s 超时、失败指数退避）拉取 job 列表/构建历史/节点/队列四类数据，计算 9 个指标并缓存为内存快照；`/metrics` 只返回内存快照，绝不在抓取时调 Jenkins。水位线等状态原子写入 PVC `state.json`，首次启动对既有构建只建基线、不产生历史计数。

## 运行方式

### 本地构建运行

```bash
go build ./cmd/exporter
# 在线模式（需 Jenkins 只读 token）
JENKINS_URL=http://jenkins.jenkins.svc.cluster.local:8080 \
JENKINS_USER=reader \
JENKINS_TOKEN_FILE=/path/to/token \
JOB_WHITELIST=ci-gate-pr,pkg-build \
./exporter
```

### Docker 冒烟（离线模式，无需 token）

```bash
docker build -t jenkins-exporter:smoke .
docker run --rm -p 9300:9300 -e OFFLINE=true -e JOB_WHITELIST=ci-gate-pr,pkg-build jenkins-exporter:smoke
curl -s localhost:9300/metrics
curl -s localhost:9300/healthz   # ok
```

离线模式用 `testdata/` 下的脱敏 JSON fixture 跑全流程，便于无 token 时本地/镜像冒烟。

## 配置项

| 配置项 | 环境变量 | 默认值 | 必填 | 说明 |
| --- | --- | --- | --- | --- |
| Jenkins 地址 | `JENKINS_URL` | — | 在线必填 | 如 `http://jenkins.jenkins.svc.cluster.local:8080` |
| Jenkins 用户 | `JENKINS_USER` | — | 在线必填 | Basic 认证用户名 |
| token 文件 | `JENKINS_TOKEN_FILE` | `/vault/secrets/jenkins-token` | 在线必填 | 文件卷挂载；进程读文件，不打日志 |
| 刷新间隔 | `SCRAPE_INTERVAL` | `60s` | 否 | 后台刷新周期 |
| job 白名单 | `JOB_WHITELIST` | — | 是 | 逗号分隔的 job 全路径，如 `folder/sub/job1,job2` |
| 状态文件 | `STATE_PATH` | `/data/state.json` | 否 | PVC 上水位线文件 |
| 监听地址 | `LISTEN_ADDR` | `:9300` | 否 | /metrics 监听 |
| 单请求超时 | `HTTP_TIMEOUT` | `10s` | 否 | 单个 Jenkins 请求超时 |
| 离线模式 | `OFFLINE` | `false` | 否 | 用 testdata fixture 离线跑 |
| fixture 目录 | `FIXTURE_DIR` | `testdata` | 否 | 离线 fixture 目录 |

> token 轮转：client 按文件 mtime 变化重读，无需重启。日志只打印 `len` 与 `sha256[:8]`，绝不打印原值。

## 指标（7 + 2）

| 指标 | 类型 | 标签 | 说明 |
| --- | --- | --- | --- |
| `jenkins_build_result_total` | counter | `job`, `result` | result ∈ {SUCCESS,FAILURE,ABORTED,UNSTABLE}；每个新 finished 构建按 result 累加 1 |
| `jenkins_build_duration_seconds` | histogram | `job` | 桶 60/300/600/1800/3600/7200/14400；观察 duration/1000 秒 |
| `jenkins_job_consecutive_failures` | gauge | `job` | 连续 FAILURE 次数；SUCCESS 清零，UNSTABLE/ABORTED 中性不变 |
| `jenkins_job_last_success_timestamp_seconds` | gauge | `job` | 最近 SUCCESS 构建时间（秒） |
| `jenkins_agent_online` | gauge | `agent` | 1 在线，0 离线（temporarilyOffline 不影响 online 判定） |
| `jenkins_executor_busy` | gauge | 无 | 全局忙碌 executor 数 |
| `jenkins_queue_length` | gauge | 无 | 构建队列长度 |
| `jenkins_exporter_scrape_success` | gauge | 无 | 自监控：最近一次刷新是否成功 |
| `jenkins_exporter_last_refresh_timestamp_seconds` | gauge | 无 | 自监控：最近一次刷新完成时间（秒） |

**连续失败计数规则：** `FAILURE` → +1；`SUCCESS` → 清零；`UNSTABLE`/`ABORTED` → 不变（中性）；未知 result → 不参与计数但推进水位线。

**硬约束：** build number 不作为标签；首次启动只建基线、不产生历史计数。

日志与指标均经 [obs-sdk](https://github.com/opensourceways/obs-sdk) 装配：日志为单行扁平 JSON 写 stdout（`time/level/msg/service/env/instance/community` + 业务字段）；每条指标序列自动附加 `service` / `env` / `instance` / `community` 常驻标签。`service` 固定为 `jenkins-exporter`，`env` / `community` 由部署注入 `OBS_ENV` / `OBS_COMMUNITY`（未注入时回落 `unknown`），`instance` 默认取 Pod 名（不注入 `OBS_INSTANCE`）。

## 本地调试

### testdata 离线跑

`testdata/` 下有脱敏 fixture（`jobs.json`/`builds.json`/`computer.json`/`queue.json`），`OFFLINE=true` 时 collector 直接读 fixture 跑全流程：

```bash
OFFLINE=true JOB_WHITELIST=ci-gate-pr,pkg-build ./exporter
curl -s localhost:9300/metrics
```

### 单测

```bash
go test ./... -cover
```

覆盖 client 解析、超时、401、空构建历史、folder 下钻、delta 计算（基线/增量/连续失败规则/漏计 warn）、state 原子写与损坏自愈、指标序列化（HELP/TYPE）。

### state.json 排障

`STATE_PATH` 指向的文件可直接查看（每 job 的水位线/连续失败/最近成功），辅助排障。

### 用本地 mock Jenkins 调试

用 `httptest` 桩（见 `internal/jenkins/client_test.go` 的 `muxHandler`）模拟 Jenkins 四类路径，复用联调逻辑。

## 部署

`deploy/` 下提供示例清单（`kubectl apply -k deploy/`）：

- `namespace.yaml` — ns `jenkins-exporter`
- `pvc.yaml` — 1Gi RWO（state.json）
- `configmap.yaml` — 非密配置
- `secretdefinition.yaml` — token 由 secrets-manager 从 Vault 同步成 Secret，再文件挂载（不用 envFrom）
- `deployment.yaml` — 1 副本 + Recreate、非 root、readOnlyRootFilesystem、drop ALL、探针 `GET /metrics`、资源 50m/100Mi→200m/200Mi
- `service.yaml` — 端口名 `http`
- `networkpolicy.yaml` — egress 仅到 Jenkins Service，ingress 仅来自 monitor-agent ns

## 约束

- 全链路只读：不写 Jenkins、不装插件、不改 job 配置
- 禁止把任何真实 token/地址凭据提交进仓库，测试用 `testdata/*.json` 脱敏样例
- 代码注释与文档使用中文
