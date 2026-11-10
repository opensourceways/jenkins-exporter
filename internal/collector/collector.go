package collector

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/opensourceways/jenkins-exporter/internal/jenkins"
	"github.com/opensourceways/jenkins-exporter/internal/state"
)

// maxBackoff 是失败退避上限（5 分钟）。
const maxBackoff = 5 * time.Minute

// Collector 负责刷新循环：拉取 → 解析 → delta 计算 → 更新指标 → 持久化 state。
type Collector struct {
	src             jenkins.DataSource
	whitelistOrder  []string
	folderWhitelist []string
	staleThreshold  time.Duration
	state           *state.State
	statePath       string
	metrics         *Metrics
	scrapeInterval  time.Duration
	log             *slog.Logger

	// redSeen 是上一轮处于 red 的 job 全路径；alerted 是已告警且仍为 red 的 job。
	// 两者配合实现「连续两轮 red 才告警，同一故障只告警一次」。
	redSeen map[string]bool
	alerted map[string]bool
	// redJobs 记录「曾经变红过」的 job，用于在恢复时把 jenkins_job_red 置 0。
	// 只增不删：保留 0 值序列便于回看，数量以历史失败过的 job 为上限。
	redJobs map[string]bool

	// webURL 是 Jenkins 的网页根地址，用于拼 job 页面链接（生产是公网域名，
	// 与采集用的集群内 Service 地址不同）。为空时降级为不带链接。
	webURL string
}

// Option 用于给 Collector 注入可选依赖，保持既有调用签名不变。
type Option func(*Collector)

// WithWebURL 设置 Jenkins 网页根地址（如 https://ci.openeuler.openatom.cn）。
func WithWebURL(u string) Option {
	return func(c *Collector) {
		c.webURL = strings.TrimSuffix(u, "/")
	}
}

// maxRedAlertsPerRound 限制单轮输出的失败 job 告警条数，避免大规模故障时刷爆日志。
const maxRedAlertsPerRound = 50

// NewCollector 构造 Collector。state 由外部加载（可为基线空 State）。
// whitelist 为精确 job 全路径；folderWhitelist 为按 folder 聚合采集的 folder 全路径。
func NewCollector(src jenkins.DataSource, whitelist, folderWhitelist []string, st *state.State, statePath string, m *Metrics, scrapeInterval, staleThreshold time.Duration, log *slog.Logger, opts ...Option) *Collector {
	m.InitJobSeries(whitelist)
	m.InitFolderSeries(folderWhitelist)
	redSeen, alerted := st.RedMaps()
	c := &Collector{
		src:             src,
		whitelistOrder:  whitelist,
		folderWhitelist: folderWhitelist,
		staleThreshold:  staleThreshold,
		state:           st,
		statePath:       statePath,
		metrics:         m,
		scrapeInterval:  scrapeInterval,
		log:             log,
		redSeen:         redSeen,
		alerted:         alerted,
		redJobs:         make(map[string]bool),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Run 启动后台刷新循环（失败指数退避，成功按 scrapeInterval）。阻塞直到 ctx 取消。
func (c *Collector) Run(ctx context.Context) {
	failures := 0
	for {
		ok := c.RefreshOnce(ctx)
		var sleep time.Duration
		if ok {
			failures = 0
			sleep = c.scrapeInterval
		} else {
			failures++
			sleep = c.backoff(failures)
			c.log.Warn("刷新失败，退避后重试", "failures", failures, "backoff_seconds", int(sleep.Seconds()))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
	}
}

// backoff 计算失败退避：base*2^failures（base=scrapeInterval）+ 抖动，上限 5 分钟。
func (c *Collector) backoff(failures int) time.Duration {
	if failures <= 0 {
		return c.scrapeInterval
	}
	n := uint(failures - 1)
	if n > 20 {
		n = 20
	}
	d := time.Duration(float64(c.scrapeInterval) * math.Pow(2, float64(n)))
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	d += time.Duration(rand.Int64N(int64(time.Second)))
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// RefreshOnce 执行一次完整刷新，返回是否整体成功。
// 整体成功 = job 列表（配置了精确白名单时）、folder 聚合、computer、queue 拉取均成功，
// 且（有精确白名单时）至少一个 job 构建拉取成功。
func (c *Collector) RefreshOnce(ctx context.Context) bool {
	if err := ctx.Err(); err != nil {
		c.finalize(false)
		return false
	}

	// 精确 job 路径：直接按配置的全路径拉取构建。
	// 不再从根递归枚举整棵 job 树 —— cn4 光 multiarch/src-openeuler 就有 4.6 万 job，
	// 为了几个重点 job 去遍历全树是分钟级、数 MB 的开销。
	// 配置了但 Jenkins 不存在的路径会在 Builds 处失败并 warn，不影响其它 job。
	var targets []string
	jobBuilds := make(map[string][]jenkins.Build)
	jobOK := 0
	if len(c.whitelistOrder) > 0 {
		targets = c.whitelistOrder
		jobBuilds = make(map[string][]jenkins.Build, len(targets))
		for _, job := range targets {
			builds, berr := c.src.Builds(ctx, job)
			if berr != nil {
				c.log.Warn("拉取 job 构建失败，跳过", "job", job, "err", berr.Error())
				continue
			}
			jobBuilds[job] = builds
			jobOK++
		}
	}

	// folder 聚合路径：每个配置的 folder 一次请求，内存聚合，不产生 per-job series。
	foldersOK, currentRed, redBuilds := c.refreshFolders(ctx)
	// 只有全部 folder 都拉取成功时才推进 red 状态机，否则会把抓取失败的 folder 误判为「已恢复」。
	if foldersOK && len(c.folderWhitelist) > 0 {
		c.processRedTransitions(currentRed)
	}
	// 失败 job 的身份指标与状态机同源，同样只在拉取成功时更新，避免整批置 0。
	if foldersOK && len(c.folderWhitelist) > 0 {
		c.syncRedJobSeries(currentRed, redBuilds)
	}

	computers, cerr := c.src.Computers(ctx)
	computersOK := cerr == nil
	if cerr != nil {
		c.log.Warn("拉取 computer 失败", "err", cerr.Error())
	}

	items, qerr := c.src.Queue(ctx)
	queueOK := qerr == nil
	if qerr != nil {
		c.log.Warn("拉取 queue 失败", "err", qerr.Error())
	}

	success := foldersOK && computersOK && queueOK && (len(targets) == 0 || jobOK > 0)

	c.processAndPersist(jobBuilds, computers, computersOK, items, queueOK)
	c.finalize(success)
	return success
}

// refreshFolders 逐个 folder 拉取直接子 job 摘要并聚合。
// 返回是否全部 folder 成功、本轮所有 red job 的全路径集合（供失败告警使用），
// 以及这些 red job 最近一次失败构建的编号（供大盘直达构建页，取不到时为 0）。
func (c *Collector) refreshFolders(ctx context.Context) (bool, map[string]bool, map[string]int) {
	red := make(map[string]bool)
	redBuilds := make(map[string]int)
	if len(c.folderWhitelist) == 0 {
		return true, red, redBuilds
	}
	ok := true
	now := time.Now()
	for _, folder := range c.folderWhitelist {
		jobs, err := c.src.FolderJobs(ctx, folder)
		if err != nil {
			c.log.Warn("拉取 folder job 摘要失败", "folder", folder, "err", err.Error())
			ok = false
			continue
		}
		c.aggregateFolder(folder, jobs, now)
		for _, j := range jobs {
			if jenkins.IsFolderClass(j.Class) {
				continue
			}
			if normalizeColor(j.Color) == "red" {
				full := folder + "/" + j.Name
				red[full] = true
				if j.LastBuild != nil {
					redBuilds[full] = j.LastBuild.Number
				}
			}
		}
	}
	return ok, red, redBuilds
}

// processRedTransitions 推进失败 job 状态机并输出告警日志。
// 规则：连续两轮 red 才告警；同一 job 在恢复为绿之前只告警一次。
func (c *Collector) processRedTransitions(current map[string]bool) {
	var toAlert []string
	for job := range current {
		if c.redSeen[job] && !c.alerted[job] {
			toAlert = append(toAlert, job)
		}
	}
	sort.Strings(toAlert)

	// 已恢复的 job 移出告警集合，下次再失败可重新告警。
	for job := range c.alerted {
		if !current[job] {
			delete(c.alerted, job)
		}
	}
	for _, job := range toAlert {
		c.alerted[job] = true
	}

	shown := toAlert
	if len(shown) > maxRedAlertsPerRound {
		shown = shown[:maxRedAlertsPerRound]
	}
	for _, job := range shown {
		c.log.Warn("job 连续两轮失败", "job", job)
	}
	if rest := len(toAlert) - len(shown); rest > 0 {
		c.log.Warn("本轮另有 job 连续失败，已省略", "omitted", rest)
	}

	c.redSeen = current
	c.state.SetRedState(c.redSeen, c.alerted)
}

// aggregateFolder 把 folder 直接子 job 的状态聚合为 folder 级指标。
// 只统计叶子 job；子 folder 本身不计数（需覆盖时显式列入 FOLDER_WHITELIST）。
func (c *Collector) aggregateFolder(folder string, jobs []jenkins.Job, now time.Time) {
	counts := make(map[string]int, len(JenkinsColorValues))
	total, stale := 0, 0
	var newest, newestSuccess int64
	for _, j := range jobs {
		if jenkins.IsFolderClass(j.Class) {
			continue
		}
		total++
		counts[normalizeColor(j.Color)]++
		if j.LastSuccessfulBuild != nil && j.LastSuccessfulBuild.Timestamp > newestSuccess {
			newestSuccess = j.LastSuccessfulBuild.Timestamp
		}
		if j.LastBuild != nil {
			if j.LastBuild.Timestamp > newest {
				newest = j.LastBuild.Timestamp
			}
			if now.Sub(time.UnixMilli(j.LastBuild.Timestamp)) <= c.staleThreshold {
				continue
			}
		}
		// 从未构建，或最近一次构建早于停滞阈值。
		stale++
	}

	c.metrics.FolderJobsTotal.Set(float64(total), folder)
	for _, color := range JenkinsColorValues {
		c.metrics.FolderJobsByColor.Set(float64(counts[color]), folder, color)
	}
	if newest > 0 {
		c.metrics.FolderLastBuildAge.Set(now.Sub(time.UnixMilli(newest)).Seconds(), folder)
	} else {
		c.metrics.FolderLastBuildAge.Set(0, folder)
	}
	if newestSuccess > 0 {
		c.metrics.FolderLastSuccessAge.Set(now.Sub(time.UnixMilli(newestSuccess)).Seconds(), folder)
	} else {
		c.metrics.FolderLastSuccessAge.Set(0, folder)
	}
	c.metrics.FolderStaleJobs.Set(float64(stale), folder)
}

// normalizeColor 归一化 Jenkins color：去掉 _anime（构建中）后缀，未知值归为 unknown。
func normalizeColor(color string) string {
	if color == "" {
		return "unknown"
	}
	color = strings.TrimSuffix(color, "_anime")
	for _, c := range JenkinsColorValues {
		if c == color {
			return color
		}
	}
	return "unknown"
}

// processAndPersist 执行 delta 计算、指标更新与 state 持久化。
func (c *Collector) processAndPersist(jobBuilds map[string][]jenkins.Build, computers []jenkins.Computer, computersOK bool, items []jenkins.QueueItem, queueOK bool) {
	for job, builds := range jobBuilds {
		c.processJob(job, builds)
	}
	if computersOK {
		c.updateComputers(computers)
	}
	if queueOK {
		c.metrics.QueueLength.Set(float64(len(items)))
		c.metrics.QueueOldestWait.Set(oldestQueueWaitSeconds(items, time.Now()))
	}
	if err := c.state.Save(c.statePath); err != nil {
		c.log.Error("持久化 state 失败", "err", err.Error())
	}
}

// finalize 设置自监控指标（始终调用，即便刷新失败）。
func (c *Collector) finalize(success bool) {
	v := 0.0
	if success {
		v = 1.0
	}
	c.metrics.ExporterScrapeSuccess.Set(v)
	c.metrics.ExporterLastRefreshTime.Set(float64(time.Now().Unix()))
}

// syncRedJobSeries 把当前 red 的 job 身份写入指标：red=1，已恢复=0。
// 只有曾经变红的 job 才会产生序列，因此 series 数上限是「历史失败过的 job 数」，
// 与 folder 聚合一次拿到的数据同源，不额外请求 Jenkins。
// 同时写入最近一次失败的构建号，供大盘直接跳到该构建的日志页。
func (c *Collector) syncRedJobSeries(current map[string]bool, builds map[string]int) {
	for job := range current {
		c.metrics.JobRed.Set(1, job)
		if n := builds[job]; n > 0 {
			c.metrics.JobLastFailedBuild.Set(float64(n), job, c.jobURL(job))
		}
		c.redJobs[job] = true
	}
	for job := range c.redJobs {
		if !current[job] {
			c.metrics.JobRed.Set(0, job)
			c.metrics.JobLastFailedBuild.Set(0, job, c.jobURL(job))
		}
	}
}

// jobURL 把 job 全路径转成 Jenkins 网页地址（不含构建号）。
// Jenkins 的 URL 规则是每一级目录都加 /job/ 前缀，例如
// multiarch/openeuler/x86-64/golang → {web}/job/multiarch/job/openeuler/job/x86-64/job/golang。
// webURL 未配置时返回空串，指标里保留空标签，大盘上的链接会失效但不影响数值。
func (c *Collector) jobURL(job string) string {
	if c.webURL == "" || job == "" {
		return ""
	}
	return c.webURL + "/job/" + strings.ReplaceAll(job, "/", "/job/")
}

// processJob 处理单个 job 的构建：基线或增量。
func (c *Collector) processJob(job string, builds []jenkins.Build) {
	finished := finishedBuilds(builds)
	sort.Slice(finished, func(i, j int) bool { return finished[i].Number < finished[j].Number })

	st := c.state.Get(job)
	if st == nil {
		// 基线：只建水位线，不累加 counter/histogram。
		st = c.state.Ensure(job)
		c.applyBaseline(job, st, finished)
		return
	}
	c.applyIncremental(job, st, builds, finished)
}

// applyBaseline 对既有构建只建水位线：last_seen_build=最大 finished number，
// consecutive_failures=0，last_success_timestamp=最近 SUCCESS 构建时间（无则 0）。
func (c *Collector) applyBaseline(job string, st *state.JobState, finished []jenkins.Build) {
	if len(finished) > 0 {
		st.LastSeenBuild = finished[len(finished)-1].Number
		var maxSuccessTs int64
		for _, b := range finished {
			if b.Result != nil && *b.Result == jenkins.ResultSuccess && b.Timestamp > maxSuccessTs {
				maxSuccessTs = b.Timestamp
			}
		}
		st.LastSuccessTimestamp = maxSuccessTs / 1000
	}
	st.ConsecutiveFailures = 0
	c.metrics.JobConsecutiveFailures.Set(0, job)
	c.metrics.JobLastSuccessTimestamp.Set(float64(st.LastSuccessTimestamp), job)
}

// applyIncremental 处理水位线之后的新 finished 构建，累加指标并推进水位线。
func (c *Collector) applyIncremental(job string, st *state.JobState, allBuilds []jenkins.Build, finished []jenkins.Build) {
	for _, b := range finished {
		if b.Number <= st.LastSeenBuild {
			continue
		}
		result := ""
		if b.Result != nil {
			result = *b.Result
		}
		if jenkins.IsKnownResult(result) {
			c.metrics.BuildResultTotal.Inc(job, result)
			c.metrics.BuildDurationSeconds.Observe(float64(b.Duration)/1000.0, job)
			switch result {
			case jenkins.ResultSuccess:
				st.ConsecutiveFailures = 0
				st.LastSuccessTimestamp = b.Timestamp / 1000
			case jenkins.ResultFailure:
				st.ConsecutiveFailures++
			case jenkins.ResultAborted, jenkins.ResultUnstable:
				// 中性：不变
			}
		}
		// 未知 result：不参与计数，仅随水位线推进避免重复处理。
	}

	// 漏计检测：返回了上限 100 条且最老一条 number 仍 > last_seen，可能存在未取回的更早新构建。
	if len(allBuilds) == 100 {
		minNum := minBuildNumber(allBuilds)
		if minNum > st.LastSeenBuild {
			c.log.Warn("可能漏计：job 新构建超过 100 条上限", "job", job, "oldest_returned", minNum, "last_seen", st.LastSeenBuild)
		}
	}

	// 水位线只进不退：仅当可得最大 finished number 超过当前水位线时才推进，
	// 避免 Jenkins 返回不全（构建删除/API 间歇/索引重建）时水位线回退导致重复计数。
	if len(finished) > 0 {
		maxNum := finished[len(finished)-1].Number
		if maxNum > st.LastSeenBuild {
			st.LastSeenBuild = maxNum
		}
	}
	c.metrics.JobConsecutiveFailures.Set(float64(st.ConsecutiveFailures), job)
	c.metrics.JobLastSuccessTimestamp.Set(float64(st.LastSuccessTimestamp), job)
}

func (c *Collector) updateComputers(comps []jenkins.Computer) {
	var busy, total int
	for _, comp := range comps {
		v := 0.0
		if !comp.Offline {
			v = 1.0
		}
		c.metrics.AgentOnline.Set(v, comp.DisplayName)
		busy += comp.BusyExecutors
		total += comp.NumExecutors
	}
	c.metrics.ExecutorBusy.Set(float64(busy))
	c.metrics.ExecutorTotal.Set(float64(total))
}

// oldestQueueWaitSeconds 返回队列中最早入队项已等待的秒数；队列为空或时间戳异常时返回 0。
func oldestQueueWaitSeconds(items []jenkins.QueueItem, now time.Time) float64 {
	var oldest int64
	for _, it := range items {
		if it.InQueueSince > 0 && (oldest == 0 || it.InQueueSince < oldest) {
			oldest = it.InQueueSince
		}
	}
	if oldest == 0 {
		return 0
	}
	wait := now.Sub(time.UnixMilli(oldest)).Seconds()
	if wait < 0 {
		return 0
	}
	return wait
}

// finishedBuilds 过滤出 result 非 nil（已完成）的构建。
func finishedBuilds(builds []jenkins.Build) []jenkins.Build {
	out := make([]jenkins.Build, 0, len(builds))
	for _, b := range builds {
		if b.Result != nil {
			out = append(out, b)
		}
	}
	return out
}

func minBuildNumber(builds []jenkins.Build) int {
	if len(builds) == 0 {
		return 0
	}
	m := builds[0].Number
	for _, b := range builds {
		if b.Number < m {
			m = b.Number
		}
	}
	return m
}

// State 暴露内部 state（供测试断言）。
func (c *Collector) State() *state.State { return c.state }
