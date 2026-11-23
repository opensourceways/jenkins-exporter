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

// deadJobThreshold 定义「废弃 job」：最后一次构建距今超过一年。
// 由于构建时间必然不早于成功时间，超过一年的 job 在过去一年里既没有成功也没有构建，
// 按成功处理，避免长期占用红色/停滞统计并让大盘出现以年为单位的陈旧数据。
const deadJobThreshold = 365 * 24 * time.Hour

// isDeadJob 判断 job 是否已废弃。从未构建（没有 lastBuild 或时间戳为 0）的 job
// 缺少时间基准，无法判断存在时长，仍按 notbuilt 口径统计。
func isDeadJob(j *jenkins.Job, now time.Time) bool {
	return j.LastBuild != nil && j.LastBuild.Timestamp > 0 &&
		now.Sub(time.UnixMilli(j.LastBuild.Timestamp)) > deadJobThreshold
}

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
	// lastBuildJobs 是上一轮输出过 last_build 时间戳的 job，用于把已从 folder 移除的 job 置 0，
	// 避免陈旧时间戳随时间推移误触发「超过一天没有执行」告警。
	lastBuildJobs map[string]bool
	// redStreaks 缓存红色 job 的连续失败次数，键为 job 全路径。
	// folder 摘要每轮都会带上 lastBuild 编号，只有编号变化才回源拉一次构建历史，
	// 稳态下不产生额外 Jenkins 请求。
	redStreaks map[string]redStreak

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

// maxStreakFetchPerRound 限制单轮回源拉取构建历史的 red job 数，
// 避免大面积失败时对 Jenkins 造成成百上千次额外请求。
const maxStreakFetchPerRound = 200

// redStreak 是单个失败 job 的连续失败次数缓存。
// lastBuildNumber 与 folder 摘要同源，用于判断是否需要回源重算。
type redStreak struct {
	lastBuildNumber int
	failures        int
}

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
		lastBuildJobs:   make(map[string]bool),
		redStreaks:      make(map[string]redStreak),
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
// 整体成功 = computer、queue 拉取成功，（有精确白名单时）至少一个 job 构建拉取成功，
// 且（配置了 folder 时）至少一个 folder 拉到数据。单个 folder 失败只影响该 folder，
// 其陈旧由 jenkins_folder_scrape_success 暴露，不把全局自监控置 0。
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
	snap := c.refreshFolders(ctx)
	if len(c.folderWhitelist) > 0 {
		// 单个 folder 抖动不能冻结全局：成功 folder 以本轮为准，失败 folder 沿用上一轮状态。
		currentRed, redLastBuild := c.mergeRedState(snap)
		// 两轮确认只认「实际拉到」的 red：失败 folder 沿用过来的 job 不算新的一次观测。
		c.processRedTransitions(currentRed, snap.red)
		c.syncRedJobSeries(currentRed, redLastBuild)
		c.syncRedJobStreaks(ctx, currentRed, redLastBuild)
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

	// 单个 folder 抖动不等于整体失明：只要还有 folder 拿到数据，全局自监控就保持成功，
	// 失败 folder 的陈旧由 jenkins_folder_scrape_success=0 暴露；全部 folder 都失败才置 0。
	foldersAllFailed := len(c.folderWhitelist) > 0 && len(snap.okFolders) == 0
	success := !foldersAllFailed && computersOK && queueOK && (len(targets) == 0 || jobOK > 0)

	c.processAndPersist(jobBuilds, computers, computersOK, items, queueOK)
	c.finalize(success)
	return success
}

// folderSnapshot 是一轮 folder 聚合的结果。
// allOK 表示本轮所有 folder 都拉到，供 mergeRedState 选择快路径；
// 全局自监控不再以它为准（单个 folder 失败不算整体失明）。
// okFolders 记录本轮成功的 folder，调用方据此只推进这些 folder 的状态。
type folderSnapshot struct {
	allOK     bool
	okFolders map[string]bool
	red       map[string]bool
	lastBuild map[string]*jenkins.BuildSummary
}

// refreshFolders 逐个 folder 拉取直接子 job 摘要并聚合。
// 某个 folder 失败只影响它自己：本轮其余 folder 的聚合与 red 集合照常产出，
// 失败 folder 由调用方沿用上一轮状态，并通过 jenkins_folder_scrape_success=0 暴露。
func (c *Collector) refreshFolders(ctx context.Context) folderSnapshot {
	snap := folderSnapshot{
		allOK:     true,
		okFolders: make(map[string]bool, len(c.folderWhitelist)),
		red:       make(map[string]bool),
		lastBuild: make(map[string]*jenkins.BuildSummary),
	}
	if len(c.folderWhitelist) == 0 {
		return snap
	}
	now := time.Now()
	currentJobs := make(map[string]bool)
	for _, folder := range c.folderWhitelist {
		jobs, err := c.src.FolderJobs(ctx, folder)
		if err != nil {
			c.log.Warn("拉取 folder job 摘要失败，沿用上一轮状态", "folder", folder, "err", err.Error())
			snap.allOK = false
			c.metrics.FolderScrapeSuccess.Set(0, folder)
			continue
		}
		snap.okFolders[folder] = true
		c.metrics.FolderScrapeSuccess.Set(1, folder)
		c.aggregateFolder(folder, jobs, now, currentJobs)
		for _, j := range jobs {
			if jenkins.IsFolderClass(j.Class) {
				continue
			}
			// 废弃 job 已按成功处理，不再进入红色集合，避免大盘出现以年为单位的失败时长。
			if isDeadJob(&j, now) {
				continue
			}
			if isFailedJob(&j) {
				full := folder + "/" + j.Name
				snap.red[full] = true
				snap.lastBuild[full] = j.LastBuild
			}
		}
	}
	// 只清理「所属 folder 本轮成功、且 job 已消失」的 last_build 序列：
	// 失败 folder 无法区分「job 被删」与「这轮没拉到」，必须原样保留。
	if len(snap.okFolders) > 0 {
		next := make(map[string]bool, len(currentJobs)+len(c.lastBuildJobs))
		for job := range currentJobs {
			next[job] = true
		}
		for job := range c.lastBuildJobs {
			if next[job] {
				continue
			}
			if !snap.okFolders[c.folderOf(job)] {
				next[job] = true
				continue
			}
			c.metrics.JobLastBuildTimestamp.Set(0, job)
		}
		c.lastBuildJobs = next
	}
	return snap
}

// mergeRedState 生成本轮用于推进状态机的 red 集合。
// 成功的 folder 以本轮结果为准（可能新增或恢复）；抓取失败的 folder 沿用上一轮的 red 状态，
// 既不会因为一个 folder 抖动就冻结全局，也不会把「没拉到」误判为「已恢复」。
// 沿用过来的 job 没有本轮的 lastBuild，指标保留上一轮的构建号。
func (c *Collector) mergeRedState(snap folderSnapshot) (map[string]bool, map[string]*jenkins.BuildSummary) {
	if snap.allOK {
		return snap.red, snap.lastBuild
	}
	merged := make(map[string]bool, len(snap.red)+len(c.redSeen))
	for job := range snap.red {
		merged[job] = true
	}
	for job := range c.redSeen {
		if merged[job] || snap.okFolders[c.folderOf(job)] {
			continue
		}
		merged[job] = true
	}
	return merged, snap.lastBuild
}

// folderOf 返回 job 全路径所属的 FOLDER_WHITELIST 条目（最长前缀匹配）。
// 找不到匹配时返回空串，调用方会保守地按「所属 folder 未成功」保留原状态。
func (c *Collector) folderOf(job string) string {
	best := ""
	for _, f := range c.folderWhitelist {
		if len(f) >= len(job) || job[len(f)] != '/' || !strings.HasPrefix(job, f) {
			continue
		}
		if len(f) > len(best) {
			best = f
		}
	}
	return best
}

// processRedTransitions 推进失败 job 状态机并输出告警日志。
// 规则：连续两轮实际拉到 red 才告警；同一 job 在恢复为绿之前只告警一次。
// current 是合并后的 red 集合（含失败 folder 沿用过来的 job），fresh 是本轮真实拉到的 red；
// 只有 fresh 里的 job 才算「新的一次观测」，避免抓取失败也被当成确认。
func (c *Collector) processRedTransitions(current, fresh map[string]bool) {
	var toAlert []string
	for job := range current {
		if fresh[job] && c.redSeen[job] && !c.alerted[job] {
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
func (c *Collector) aggregateFolder(folder string, jobs []jenkins.Job, now time.Time, currentJobs map[string]bool) {
	counts := make(map[string]int, len(JenkinsColorValues))
	total, stale := 0, 0
	var newest, newestSuccess int64
	for _, j := range jobs {
		if jenkins.IsFolderClass(j.Class) {
			continue
		}
		total++
		full := folder + "/" + j.Name
		currentJobs[full] = true
		// 废弃 job（一年内既无成功也无构建）视为成功：计入 blue，不计停滞，
		// 也不参与最近成功/构建时间的计算，避免 folder 级指标被陈年数据拖老。
		if isDeadJob(&j, now) {
			counts["blue"]++
			// 时间戳仍按真实值输出，供需要审计的查询使用。
			c.setJobTimestamps(&j, full)
			continue
		}
		counts[normalizeColor(j.Color)]++
		if j.LastSuccessfulBuild != nil && j.LastSuccessfulBuild.Timestamp > newestSuccess {
			newestSuccess = j.LastSuccessfulBuild.Timestamp
		}
		// 与白名单 job 共用同一指标：folder 内叶子 job 也输出最近成功时间，
		// 供「连续失败超过 N 天」判断（当前 red + 距最近一次成功超过阈值）。
		c.setJobTimestamps(&j, full)
		if j.LastBuild != nil {
			if j.LastBuild.Timestamp > newest {
				newest = j.LastBuild.Timestamp
			}
			if now.Sub(time.UnixMilli(j.LastBuild.Timestamp)) <= c.staleThreshold {
				continue
			}
			// 只有「曾经构建过、但最近一次构建早于停滞阈值」才算停滞；
			// 从未构建的 job 属于 notbuilt 状态，由颜色维度单独体现，避免淹没真正的停摆 job。
			stale++
		}
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

// setJobTimestamps 输出叶子 job 的最近成功/最近构建时间戳（秒），从未发生时为 0。
func (c *Collector) setJobTimestamps(j *jenkins.Job, full string) {
	if j.LastSuccessfulBuild != nil {
		c.metrics.JobLastSuccessTimestamp.Set(float64(j.LastSuccessfulBuild.Timestamp/1000), full)
	} else {
		c.metrics.JobLastSuccessTimestamp.Set(0, full)
	}
	if j.LastBuild != nil {
		c.metrics.JobLastBuildTimestamp.Set(float64(j.LastBuild.Timestamp/1000), full)
	} else {
		c.metrics.JobLastBuildTimestamp.Set(0, full)
	}
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

// isFailedJob 判断 job 是否处于「最近一次已完成构建失败」这一可告警状态。
// Jenkins 的 color 带 _anime 后缀表示 job 正在构建：red_anime 只说明「上次构建失败、
// 现在又在重建」，此时 lastBuild 是进行中的构建（result 为 null）。若把 red_anime 当作失败，
// CI 一触发就会报失败，且构建号指向一个还没结束的构建，因此构建中一律不算失败：
// 重建成功后转绿，重建再次失败时 color 回到 red，会重新告警。
func isFailedJob(j *jenkins.Job) bool {
	if strings.HasSuffix(j.Color, "_anime") {
		return false
	}
	return normalizeColor(j.Color) == "red"
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
func (c *Collector) syncRedJobSeries(current map[string]bool, lastBuild map[string]*jenkins.BuildSummary) {
	for job := range current {
		c.metrics.JobRed.Set(1, job)
		if lb := lastBuild[job]; lb != nil && lb.Number > 0 {
			c.metrics.JobLastFailedBuild.Set(float64(lb.Number), job, c.jobURL(job))
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

// syncRedJobStreaks 为当前 red 的 folder job 计算连续 FAILURE 次数，写入
// jenkins_job_consecutive_failures（与白名单 job 复用同一指标）。
// 判据与增量状态机一致：FAILURE 累加，SUCCESS 终止，UNSTABLE/ABORTED 中性跳过。
// folder 摘要每轮都带 lastBuild 编号，只有编号变化才回源拉构建历史，
// 因此稳态下（没有任何新构建）不产生额外请求。
func (c *Collector) syncRedJobStreaks(ctx context.Context, current map[string]bool, lastBuild map[string]*jenkins.BuildSummary) {
	fetched := 0
	skipped := 0
	for job := range current {
		lb := lastBuild[job]
		if lb == nil || lb.Number <= 0 {
			continue
		}
		if s, ok := c.redStreaks[job]; ok && s.lastBuildNumber == lb.Number {
			c.metrics.JobConsecutiveFailures.Set(float64(s.failures), job)
			continue
		}
		if fetched >= maxStreakFetchPerRound {
			skipped++
			continue
		}
		fetched++
		builds, err := c.src.Builds(ctx, job)
		if err != nil {
			c.log.Warn("拉取失败 job 构建历史失败，跳过连续失败次数", "job", job, "err", err.Error())
			continue
		}
		failures := countFailureStreak(builds)
		c.redStreaks[job] = redStreak{lastBuildNumber: lb.Number, failures: failures}
		c.metrics.JobConsecutiveFailures.Set(float64(failures), job)
	}
	if skipped > 0 {
		c.log.Warn("本轮失败 job 过多，部分 job 未计算连续失败次数", "skipped", skipped)
	}
}

// countFailureStreak 从最新构建往回数连续 FAILURE 次数。
// 未完成构建（result 为 nil）跳过，SUCCESS 终止，UNSTABLE/ABORTED 视为中性继续往前。
func countFailureStreak(builds []jenkins.Build) int {
	sorted := make([]jenkins.Build, len(builds))
	copy(sorted, builds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number > sorted[j].Number })
	n := 0
	for _, b := range sorted {
		if b.Result == nil {
			continue
		}
		switch *b.Result {
		case jenkins.ResultFailure:
			n++
		case jenkins.ResultSuccess:
			return n
		}
	}
	return n
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
