# jenkins-exporter Folder 级聚合采集设计（方案 A）

> 关联需求：opensourceways/backlog#2784
> 关联 PR：#279（helm-chart-value）、#298（infrastructure）

## 1. 背景

cn4 门禁 Jenkins（`ci.openeuler.openatom.cn`）的 job 全部按 folder 组织，实测规模：

| folder | job 数 |
|---|---|
| `multiarch/src-openeuler/x86-64` | 11600 |
| `multiarch/src-openeuler/aarch64` | 11601 |
| `multiarch/src-openeuler/trigger` | 11600 |
| `multiarch/src-openeuler/comment` | 11600 |
| `multiarch/src-openeuler/approve` | 7 |
| `multiarch/src-openeuler/jobs-crud` | 8 |

当前 exporter 的白名单是 **job 全路径精确匹配**（`collector.go:163-182` 的 `filterWhitelist`），无法覆盖这种量级：

- 逐 job 拉 `builds[...]{0,100}`，4.6 万 job × 串行 10s 超时，单轮刷新不可完成
- 若改通配全量展开，series 数量约 8 万，Prometheus 侧压力不可接受
- 与需求「不遍历全量」的约束冲突

线上现状：`JOB_WHITELIST=ci-gate-pr` 是探针值，日志每 5 分钟刷一条 `白名单 job 在 Jenkins 不存在`。

## 2. 目标与非目标

### 目标

1. 覆盖全量门禁 job 的**状态分布**，不逐个建 series
2. 保持「只读」约束：不改 Jenkins、不装插件、不改 job 配置
3. 单轮刷新耗时可控（目标 < 30s），请求数只与配置的 folder 数相关
4. 保留原有精确 job 采集能力，向后兼容

### 非目标

- 不做 per-job 的连续失败计数（需要构建历史，与聚合路径冲突）
- 不做 folder 级构建耗时分布（folder API 只给 lastBuild duration，无法构成分布）
- 不引入 Jenkins 插件或 Prometheus 以外的存储

## 3. 现状分析

### 3.1 采集链路

```
RefreshOnce (collector.go:93)
  ├─ EnumerateLeafJobs(ctx, src, "")        递归遍历全部 folder → 4.6 万叶子全路径
  ├─ filterWhitelist(leaves)                精确匹配白名单
  ├─ for job := range targets:
  │    src.Builds(ctx, job)                 逐 job 拉 100 条构建 ← 瓶颈
  ├─ src.Computers(ctx)                     一次请求
  └─ src.Queue(ctx)                         一次请求
```

### 3.2 可复用资产

| 资产 | 位置 | 说明 |
|---|---|---|
| folder 递归下钻 | `internal/jenkins/folder.go:11` | `EnumerateLeafJobs`，已能逐层拼接全路径 |
| DataSource 抽象 | `internal/jenkins/types.go:65` | 4 个方法，client / offline 两套实现 |
| 增量水位线 | `internal/state/state.go` | 精确 job 路径使用 |
| 指标装配 | `internal/collector/metrics.go:29` | obs-sdk 统一附加 service/env/instance 标签 |

### 3.3 关键实测数据

folder 级 API 一次性拉全量子 job 摘要：

| 请求 | 大小 | 耗时 |
|---|---|---|
| `.../x86-64/api/json?tree=jobs[name,color]` | 977 KB | 0.47s |
| `.../x86-64/api/json?tree=jobs[name,color,lastBuild[number,result,duration,timestamp],lastSuccessfulBuild[number,timestamp]]` | 2.3 MB | 0.50s |

**结论：一次请求即可拿到 11600 个 job 的最近构建摘要，聚合路径在技术上完全可行。**

## 4. 设计方案

### 4.1 配置模型

新增环境变量，与现有 `JOB_WHITELIST` 并存、互不干扰：

| 变量 | 语义 | 示例 |
|---|---|---|
| `JOB_WHITELIST` | 精确 job 全路径（保持现状） | `multiarch/src-openeuler/approve/xxx` |
| `FOLDER_WHITELIST` | folder 全路径前缀（新增） | `multiarch/src-openeuler/x86-64,multiarch/src-openeuler/aarch64` |
| `STALE_THRESHOLD` | 超过多久未构建算「停滞」，默认 `24h`（新增） | `24h` |

两者都配置时，两套指标同时输出；都为空时不采集 job 维度数据（仅保留 computer / queue / 自监控）。

`FOLDER_WHITELIST` 不做递归展开，**只统计该 folder 的直接子 job**。需要覆盖多层时显式列出多个 folder，避免一次配出不可控的数据量。

### 4.2 采集路径

```
RefreshOnce
  ├─ 精确 job 路径（原有逻辑，仅当 JOB_WHITELIST 非空）
  │    EnumerateLeafJobs → filterWhitelist → 逐 job Builds（保留）
  ├─ folder 聚合路径（新增，仅当 FOLDER_WHITELIST 非空）
  │    for folder := range folderWhitelist:
  │      src.FolderJobs(ctx, folder)     一次请求，取该 folder 全部子 job 摘要
  │      aggregateFolder(folder, jobs)   内存聚合，不产生 per-job series
  ├─ src.Computers(ctx)
  └─ src.Queue(ctx)
```

**关键优化**：现有 `EnumerateLeafJobs(ctx, src, "")` 会从顶层递归遍历全部 folder。新增路径改为**只遍历配置的前缀**，不触碰未配置的 folder，请求数从「全部 folder 数」降到「配置数 + 路径深度」。

### 4.3 指标定义

新增 5 个 folder 级指标（全部为 gauge，快照型，无需 state 水位线）：

| 指标 | 标签 | 含义 |
|---|---|---|
| `jenkins_folder_jobs_total` | `folder` | 该 folder 下的 job 总数 |
| `jenkins_folder_jobs_by_color` | `folder`, `color` | 按 Jenkins color 分类的 job 数 |
| `jenkins_folder_last_build_age_seconds` | `folder` | 该 folder 内最近一次构建距今秒数（0 表示无构建） |
| `jenkins_folder_last_success_age_seconds` | `folder` | 该 folder 内最近一次**成功**构建距今秒数（0 表示从未成功）。与上一条配合可识别「一直在跑但一直失败」的 job——这类 job 的 last_build_age 很小，看起来健康 |
| `jenkins_folder_stale_jobs` | `folder` | 超过 `STALE_THRESHOLD` 未构建的 job 数 |

同时补齐 2 个「数据已在手但未输出」的全局指标（零新增请求）：

| 指标 | 标签 | 含义 |
|---|---|---|
| `jenkins_executor_total` | 无 | 各节点 `numExecutors` 之和，与 `executor_busy` 相除即饱和度 |
| `jenkins_queue_oldest_wait_seconds` | 无 | 队列中最早入队项（`inQueueSince`）已等待秒数；队列为空时为 0 |

`color` 取值与语义（Jenkins 原生）：

| color | 含义 |
|---|---|
| `blue` | 最近构建成功 |
| `red` | 最近构建失败 |
| `yellow` | 最近构建不稳定 |
| `aborted` | 最近构建被中止 |
| `notbuilt` | 从未构建 |
| `disabled` | 已禁用 |

series 数估算：4 个 folder ×（1 个总数 + 7 个 color + 3 个 age/stale）= **44 条**，对比 per-job 方案 8 万条。

### 4.4 失败 job 定位告警（日志方案）

folder 聚合刻意不暴露 job 身份，因此无法通过指标告警到具体 job。补一条**日志侧**的通道，不新增任何 Prometheus series、不新增任何 API 请求：

**状态机**（持久化在 state.json）：

| 字段 | 含义 |
|---|---|
| `red_seen` | 上一轮处于 red 的 job 全路径集合 |
| `alerted` | 已告警且仍为 red 的 job 集合 |

每轮 folder 聚合完成后：

```
current   = 本轮所有 color=red 的 job 全路径
toAlert   = (current ∩ red_seen) - alerted     // 连续两轮 red 且未告警过
alerted   = (alerted ∩ current) ∪ toAlert      // 仍为 red 的保留，恢复的移出
red_seen  = current
```

规则：

- **连续两轮 red 才告警** → 过滤单轮抖动（flaky job）
- **同一故障只告警一次** → 恢复为绿前不重复；恢复后再红重新走两轮判定
- **每轮上限 50 条** → 超出汇总一条「另有 N 个」，避免故障风暴刷屏
- **folder 拉取失败时跳过状态机** → 避免把「没采到」误判为「已恢复」

日志：

```json
{"level":"WARN","msg":"job 连续两轮失败","job":"multiarch/src-openeuler/x86-64/xxx"}
```

Loki 直接消费该日志做告警，通知可带 job 名。成本：0 次 API 请求、<1% CPU、state 增加约几十 KB / 轮。

**与需求条目的映射**：

| 需求关注点 | 对应指标 |
|---|---|
| 门禁失败率 | `jenkins_folder_jobs_by_color{color="red"}` / `jobs_total` |
| 构建变慢 | 暂不覆盖（见 §7 后续演进） |
| agent 掉线 | 沿用现有 `jenkins_agent_online` |
| 连续失败 | 用 `jobs_by_color{color="red"}` 的快照替代，配合 `changes()` 观察劣化 |

### 4.4 数据结构与接口变更

`internal/jenkins/types.go`：

```go
// Job 扩展构建摘要字段（可选，仅 FolderJobs 填充）
type Job struct {
    Name  string `json:"name"`
    Class string `json:"_class"`
    URL   string `json:"url"`
    Color string `json:"color"`
    LastBuild            *BuildSummary `json:"lastBuild"`
    LastSuccessfulBuild  *BuildSummary `json:"lastSuccessfulBuild"`
}

// BuildSummary 是 folder 列表接口返回的构建摘要（字段少于 Build）
type BuildSummary struct {
    Number    int     `json:"number"`
    Result    *string `json:"result"`
    Duration  int64   `json:"duration"`
    Timestamp int64   `json:"timestamp"`
}

// DataSource 新增一个方法
type DataSource interface {
    Jobs(ctx context.Context, path string) ([]Job, error)
    FolderJobs(ctx context.Context, path string) ([]Job, error)  // 新增
    Builds(ctx context.Context, jobPath string) ([]Build, error)
    Computers(ctx context.Context) ([]Computer, error)
    Queue(ctx context.Context) ([]QueueItem, error)
}
```

`internal/jenkins/client.go` 新增：

```go
func (c *Client) FolderJobs(ctx context.Context, jobPath string) ([]Job, error) {
    const tree = "jobs[name,_class,url,color," +
        "lastBuild[number,result,duration,timestamp]," +
        "lastSuccessfulBuild[number,timestamp]]"
    full := c.jobURL(jobPath, tree)
    var resp JobsResponse
    if _, err := c.getJSON(ctx, full, &resp); err != nil {
        return nil, err
    }
    return resp.Jobs, nil
}
```

`internal/jenkins/offline.go`：同步实现 `FolderJobs`，从 fixture 的 `jobs.json` 读取，保持离线演练可用。

## 5. 代码改动清单

| 文件 | 改动 | 预估行数 |
|---|---|---|
| `internal/jenkins/types.go` | 扩展 `Job` 字段；新增 `BuildSummary`；`DataSource` 加 `FolderJobs` | +25 |
| `internal/jenkins/client.go` | 新增 `FolderJobs` 方法 | +15 |
| `internal/jenkins/offline.go` | 实现 `FolderJobs` | +20 |
| `internal/config/config.go` | 新增 `FOLDER_WHITELIST`、`STALE_THRESHOLD` 解析与默认值 | +30 |
| `internal/collector/metrics.go` | 新增 4 个指标定义与预建序列 | +25 |
| `internal/collector/collector.go` | 新增 folder 聚合分支、`aggregateFolder`、`colorOf`；`RefreshOnce` 分流 | +90 |
| `internal/collector/collector_test.go` | folder 聚合单测（正常 / 空 folder / 全 notbuilt / stale 计算） | +120 |
| `internal/jenkins/client_test.go` | `FolderJobs` 响应解析单测 | +40 |
| `cmd/exporter/main.go` | 传递新配置 | +5 |
| `README.md` | 配置说明与指标表更新 | +30 |

**合计约 400 行 diff，其中生产代码约 210 行，测试约 160 行。**

## 6. 改动量评估

**中等偏小。** 原因：

1. folder 递归下钻（`folder.go`）和 DataSource 抽象已经存在，聚合路径是**增量新增**，不重写现有逻辑
2. folder 级数据是**快照型**，不需要 state 水位线，避免了最复杂的增量计算
3. 原有精确 job 链路完全保留，新旧两条路径并行，回归风险低
4. 指标全是 gauge，obs-sdk 装配零改动

主要工作量在测试覆盖（约 40%）和文档更新，核心逻辑不足 100 行。

## 7. 风险与兼容性

| 风险 | 影响 | 应对 |
|---|---|---|
| folder 下 job 数继续增长 | 单次响应变大（当前 2.3MB/0.5s） | 加 `HTTP_TIMEOUT` 保护；超过阈值告警；必要时按子 folder 拆分配置 |
| 大 folder 响应超时 | 聚合数据缺失 | `FolderJobs` 失败时只影响该 folder，`scrape_success` 记为部分失败，不影响精确 job 路径 |
| `DataSource` 接口变更 | offline / 测试桩需同步实现 | 已在清单中列出，改动点明确 |
| Jenkins 权限收紧匿名读 | 采集 401 | 现有 token 机制不变，仅换 tree 参数 |

**兼容性**：`JOB_WHITELIST` 语义不变；`FOLDER_WHITELIST` 为空时行为与当前完全一致。

## 8. 测试计划

**单元测试**

1. `client_test.go`：`FolderJobs` 解析 color / lastBuild / lastSuccessfulBuild，含 null 场景
2. `collector_test.go`：
   - 3 个 job（blue/red/notbuilt）→ `jobs_by_color` 计数正确
   - 全 null lastBuild → `last_build_age_seconds=0`、`stale_jobs` 计入全部
   - `STALE_THRESHOLD` 边界（刚好等于阈值）
   - 空 folder → 所有指标为 0，不报错
   - `FOLDER_WHITELIST` 与 `JOB_WHITELIST` 同时配置 → 两套指标都在

**离线演练**

`OFFLINE=true` + fixture 增加一个 folder 场景，验证 `/metrics` 输出完整指标族（HELP/TYPE 齐全）。

**线上验证**

1. `FOLDER_WHITELIST=multiarch/src-openeuler/x86-64` 灰度一轮
2. 确认 `/metrics` 中 `jenkins_folder_jobs_total{folder="multiarch/src-openeuler/x86-64"}=11600`
3. 确认刷新耗时、`scrape_success=1`、无内存异常
4. 再扩展到 4 个 folder

## 9. 分阶段落地

| 阶段 | 内容 | 产出 |
|---|---|---|
| P0 | 补齐 `FolderJobs` + 配置 + 指标 + 单测 | 代码 PR |
| P1 | 灰度 `x86-64` 一个 folder | 线上指标验证 |
| P2 | 扩展到 4 个门禁 folder | values 配置 PR |
| P3（可选） | 精确 job 保留少量核心仓库，补充连续失败指标 | 后续需求 |

## 10. 后续演进（不在本次范围）

- **构建耗时**：folder API 无法给分布。可选方案是对 `approve` / `jobs-crud` 这类小 folder 走逐 job 路径采 histogram
- **连续失败 Top N**：定期全量扫描一次，只输出失败次数最高的 N 个 job，兼顾覆盖与 series 数量
