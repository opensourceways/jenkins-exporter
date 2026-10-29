package collector

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/opensourceways/jenkins-exporter/internal/jenkins"
	"github.com/opensourceways/jenkins-exporter/internal/state"
)

// maxBackoff 是失败退避上限（5 分钟）。
const maxBackoff = 5 * time.Minute

// Collector 负责刷新循环：拉取 → 解析 → delta 计算 → 更新指标 → 持久化 state。
type Collector struct {
	src            jenkins.DataSource
	whitelist      map[string]bool
	whitelistOrder []string
	state          *state.State
	statePath      string
	metrics        *Metrics
	scrapeInterval time.Duration
	log            *slog.Logger
}

// NewCollector 构造 Collector。state 由外部加载（可为基线空 State）。
func NewCollector(src jenkins.DataSource, whitelist []string, st *state.State, statePath string, m *Metrics, scrapeInterval time.Duration, log *slog.Logger) *Collector {
	wl := make(map[string]bool, len(whitelist))
	for _, j := range whitelist {
		wl[j] = true
	}
	m.InitJobSeries(whitelist)
	return &Collector{
		src:            src,
		whitelist:      wl,
		whitelistOrder: whitelist,
		state:          st,
		statePath:      statePath,
		metrics:        m,
		scrapeInterval: scrapeInterval,
		log:            log,
	}
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
// 整体成功 = job 列表、computer、queue 拉取均成功，且（有白名单时）至少一个 job 构建拉取成功。
func (c *Collector) RefreshOnce(ctx context.Context) bool {
	if err := ctx.Err(); err != nil {
		c.finalize(false)
		return false
	}

	leaves, err := jenkins.EnumerateLeafJobs(ctx, c.src, "")
	jobsOK := err == nil
	if err != nil {
		c.log.Warn("拉取 job 列表失败", "err", err.Error())
	}
	targets := c.filterWhitelist(leaves)

	jobBuilds := make(map[string][]jenkins.Build, len(targets))
	jobOK := 0
	for _, job := range targets {
		builds, berr := c.src.Builds(ctx, job)
		if berr != nil {
			c.log.Warn("拉取 job 构建失败，跳过", "job", job, "err", berr.Error())
			continue
		}
		jobBuilds[job] = builds
		jobOK++
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

	success := jobsOK && computersOK && queueOK && (len(targets) == 0 || jobOK > 0)

	c.processAndPersist(jobBuilds, computers, computersOK, items, queueOK)
	c.finalize(success)
	return success
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

func (c *Collector) filterWhitelist(leaves []string) []string {
	if len(c.whitelist) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.whitelistOrder))
	seen := map[string]bool{}
	for _, l := range leaves {
		if c.whitelist[l] && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	// 白名单中存在但 Jenkins 不存在的 job：保留以维持指标序列并 warn 一次。
	for _, w := range c.whitelistOrder {
		if !seen[w] {
			c.log.Warn("白名单 job 在 Jenkins 不存在", "job", w)
		}
	}
	return out
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
	c.metrics.JobConsecutiveFailures.WithLabelValues(job).Set(0)
	c.metrics.JobLastSuccessTimestamp.WithLabelValues(job).Set(float64(st.LastSuccessTimestamp))
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
			c.metrics.BuildResultTotal.WithLabelValues(job, result).Inc()
			c.metrics.BuildDurationSeconds.WithLabelValues(job).Observe(float64(b.Duration) / 1000.0)
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

	// 水位线推进到可得最大 finished number（running 构建不计入水位线）。
	if len(finished) > 0 {
		st.LastSeenBuild = finished[len(finished)-1].Number
	}
	c.metrics.JobConsecutiveFailures.WithLabelValues(job).Set(float64(st.ConsecutiveFailures))
	c.metrics.JobLastSuccessTimestamp.WithLabelValues(job).Set(float64(st.LastSuccessTimestamp))
}

func (c *Collector) updateComputers(comps []jenkins.Computer) {
	var busy int
	for _, comp := range comps {
		v := 0.0
		if !comp.Offline {
			v = 1.0
		}
		c.metrics.AgentOnline.WithLabelValues(comp.DisplayName).Set(v)
		busy += comp.BusyExecutors
	}
	c.metrics.ExecutorBusy.Set(float64(busy))
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
