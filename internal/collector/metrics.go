// Package collector 定义 9 个 Prometheus 指标并实现刷新循环与增量计算。
package collector

import (
	obsmetrics "github.com/opensourceways/obs-sdk/go/metrics"
)

// 指标默认桶（秒）：60/300/600/1800/3600/7200/14400。
var durationBuckets = []float64{60, 300, 600, 1800, 3600, 7200, 14400}

// Metrics 持有全部 9 个指标的定义。
// 经 obs-sdk 装配：自动附加 service/env/instance 常驻 label 与 community 可变 label。
type Metrics struct {
	BuildResultTotal        *obsmetrics.CounterVec
	BuildDurationSeconds    *obsmetrics.HistogramVec
	JobConsecutiveFailures  *obsmetrics.GaugeVec
	JobLastSuccessTimestamp *obsmetrics.GaugeVec
	AgentOnline             *obsmetrics.GaugeVec
	ExecutorBusy            *obsmetrics.GaugeVec
	QueueLength             *obsmetrics.GaugeVec
	ExporterScrapeSuccess   *obsmetrics.GaugeVec
	ExporterLastRefreshTime *obsmetrics.GaugeVec

	knownResults     []string
	defaultCommunity string
}

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
		QueueLength: om.NewGaugeVec("jenkins_queue_length",
			"Jenkins 构建队列长度。"),
		ExporterScrapeSuccess: om.NewGaugeVec("jenkins_exporter_scrape_success",
			"自监控：最近一次刷新是否全部成功（1 成功，0 失败）。"),
		ExporterLastRefreshTime: om.NewGaugeVec("jenkins_exporter_last_refresh_timestamp_seconds",
			"自监控：最近一次刷新完成时间（秒）。"),
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
