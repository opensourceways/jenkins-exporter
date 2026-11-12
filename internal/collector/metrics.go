// Package collector 定义 9 个 Prometheus 指标并实现刷新循环与增量计算。
package collector

import (
	obsmetrics "github.com/opensourceways/obs-sdk/go/metrics"
)

// 指标默认桶（秒）：60/300/600/1800/3600/7200/14400。
var durationBuckets = []float64{60, 300, 600, 1800, 3600, 7200, 14400}

// Metrics 持有全部指标的定义。
// 经 obs-sdk 装配：自动附加 service/env/instance 常驻 label 与 community 可变 label。
type Metrics struct {
	BuildResultTotal        *obsmetrics.CounterVec
	BuildDurationSeconds    *obsmetrics.HistogramVec
	JobConsecutiveFailures  *obsmetrics.GaugeVec
	JobLastSuccessTimestamp *obsmetrics.GaugeVec
	AgentOnline             *obsmetrics.GaugeVec
	ExecutorBusy            *obsmetrics.GaugeVec
	ExecutorTotal           *obsmetrics.GaugeVec
	QueueLength             *obsmetrics.GaugeVec
	QueueOldestWait         *obsmetrics.GaugeVec
	ExporterScrapeSuccess   *obsmetrics.GaugeVec
	ExporterLastRefreshTime *obsmetrics.GaugeVec

	// folder 级聚合指标：覆盖全量 job 的状态分布，不产生 per-job series。
	FolderJobsTotal      *obsmetrics.GaugeVec
	FolderJobsByColor    *obsmetrics.GaugeVec
	FolderLastBuildAge   *obsmetrics.GaugeVec
	FolderLastSuccessAge *obsmetrics.GaugeVec
	FolderStaleJobs      *obsmetrics.GaugeVec

	// JobRed 给出失败 job 的具体身份：仅对「曾经变红」的 job 出序列，恢复后置 0。
	// 全量 job 都走 folder 聚合，若逐个出序列会产生上千条无用 series；
	// 而失败 job 通常只有几十个，按需出序列既能让告警带上 job 名，又不增加任何 Jenkins 请求。
	JobRed *obsmetrics.GaugeVec

	// JobLastFailedBuild 给出失败 job 最近一次失败的构建号，值即构建编号（0 表示已恢复）。
	// job_url 是 job 页面地址（不含构建号，恒定值，不会随构建号变化产生新 series），
	// 大盘用 job_url + 值拼出构建页链接，点进去即可看失败日志。
	JobLastFailedBuild *obsmetrics.GaugeVec

	// JobLastBuildTimestamp 给出 folder 内每个叶子 job 最近一次构建的时间戳（秒），
	// 用于「超过一天没有执行」类告警；0 表示从未构建或该 job 已从 folder 移除。
	// 序列数量与 FOLDER_WHITELIST 下叶子 job 数同级，不随 Jenkins 全站 job 数膨胀。
	JobLastBuildTimestamp *obsmetrics.GaugeVec

	knownResults     []string
	defaultCommunity string
}

// JenkinsColorValues 是 color 归一化后的全部取值（去掉 _anime 后缀后）。
var JenkinsColorValues = []string{"blue", "red", "yellow", "aborted", "notbuilt", "disabled", "unknown"}

// NewMetrics 创建并注册全部指标到 om。
func NewMetrics(om *obsmetrics.Metrics) *Metrics {
	return &Metrics{
		knownResults:     []string{"SUCCESS", "FAILURE", "ABORTED", "UNSTABLE"},
		defaultCommunity: om.DefaultCommunity(),
		BuildResultTotal: om.NewCounterVec("jenkins_build_result_total",
			"按 job 与结果累计的构建总数（仅 finished 构建参与累加）。", "job", "result"),
		BuildDurationSeconds: om.NewHistogramVecWithBuckets("jenkins_build_duration_seconds",
			"构建耗时分布（秒）。", durationBuckets, "job"),
		JobConsecutiveFailures: om.NewGaugeVec("jenkins_job_consecutive_failures",
			"job 连续 FAILURE 次数；SUCCESS 清零，UNSTABLE/ABORTED 中性不变。", "job"),
		JobLastSuccessTimestamp: om.NewGaugeVec("jenkins_job_last_success_timestamp_seconds",
			"job 最近一次 SUCCESS 构建时间（秒）。", "job"),
		AgentOnline: om.NewGaugeVec("jenkins_agent_online",
			"agent 是否在线：1 在线，0 离线（temporarilyOffline 不影响 online 判定）。", "agent"),
		ExecutorBusy: om.NewGaugeVec("jenkins_executor_busy",
			"全局忙碌 executor 数。"),
		ExecutorTotal: om.NewGaugeVec("jenkins_executor_total",
			"全局 executor 总数（各节点 numExecutors 之和），与 executor_busy 相除即饱和度。"),
		QueueLength: om.NewGaugeVec("jenkins_queue_length",
			"Jenkins 构建队列长度。"),
		QueueOldestWait: om.NewGaugeVec("jenkins_queue_oldest_wait_seconds",
			"队列中最早入队项的已等待秒数；队列为空时为 0。"),
		ExporterScrapeSuccess: om.NewGaugeVec("jenkins_exporter_scrape_success",
			"自监控：最近一次刷新是否全部成功（1 成功，0 失败）。"),
		ExporterLastRefreshTime: om.NewGaugeVec("jenkins_exporter_last_refresh_timestamp_seconds",
			"自监控：最近一次刷新完成时间（秒）。"),
		FolderJobsTotal: om.NewGaugeVec("jenkins_folder_jobs_total",
			"folder 下直接子 job 总数（不含子 folder）。", "folder"),
		FolderJobsByColor: om.NewGaugeVec("jenkins_folder_jobs_by_color",
			"按 Jenkins color 分类的 job 数；_anime 后缀已归一化。", "folder", "color"),
		FolderLastBuildAge: om.NewGaugeVec("jenkins_folder_last_build_age_seconds",
			"folder 内最近一次构建距今秒数；0 表示该 folder 无任何构建。", "folder"),
		FolderLastSuccessAge: om.NewGaugeVec("jenkins_folder_last_success_age_seconds",
			"folder 内最近一次成功构建距今秒数；0 表示该 folder 从未成功过。", "folder"),
		FolderStaleJobs: om.NewGaugeVec("jenkins_folder_stale_jobs",
			"超过 STALE_THRESHOLD 未构建（或从未构建）的 job 数。", "folder"),
		JobRed: om.NewGaugeVec("jenkins_job_red",
			"job 当前是否失败（Jenkins color=red）：1 失败，0 已恢复。仅对曾经失败的 job 出序列。", "job"),
		JobLastFailedBuild: om.NewGaugeVec("jenkins_job_last_failed_build",
			"当前失败 job 最近一次失败的构建编号；0 表示已恢复。job_url 为 job 页面地址，可与编号拼出构建页链接。",
			"job", "job_url"),
		JobLastBuildTimestamp: om.NewGaugeVec("jenkins_job_last_build_timestamp_seconds",
			"folder 内叶子 job 最近一次构建的时间戳（秒）；0 表示从未构建。仅覆盖 FOLDER_WHITELIST。", "job"),
	}
}

// InitFolderSeries 为配置的 folder 预建 0 值序列，确保即使采集失败 /metrics 也有完整指标族。
func (m *Metrics) InitFolderSeries(folders []string) {
	for _, f := range folders {
		m.FolderJobsTotal.Vec().WithLabelValues(m.defaultCommunity, f).Set(0)
		for _, c := range JenkinsColorValues {
			m.FolderJobsByColor.Vec().WithLabelValues(m.defaultCommunity, f, c).Set(0)
		}
		m.FolderLastBuildAge.Vec().WithLabelValues(m.defaultCommunity, f).Set(0)
		m.FolderLastSuccessAge.Vec().WithLabelValues(m.defaultCommunity, f).Set(0)
		m.FolderStaleJobs.Vec().WithLabelValues(m.defaultCommunity, f).Set(0)
	}
}

// InitJobSeries 为白名单 job 预建 0 值序列，确保 /metrics 始终输出全部指标族（含 HELP/TYPE）。
func (m *Metrics) InitJobSeries(jobs []string) {
	for _, job := range jobs {
		for _, r := range m.knownResults {
			m.BuildResultTotal.Vec().WithLabelValues(m.defaultCommunity, job, r)
		}
		m.BuildDurationSeconds.Vec().WithLabelValues(m.defaultCommunity, job)
		m.JobConsecutiveFailures.Vec().WithLabelValues(m.defaultCommunity, job).Set(0)
		m.JobLastSuccessTimestamp.Vec().WithLabelValues(m.defaultCommunity, job).Set(0)
	}
}
