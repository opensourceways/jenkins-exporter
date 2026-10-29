// Package collector 定义 9 个 Prometheus 指标并实现刷新循环与增量计算。
package collector

import (
	"github.com/prometheus/client_golang/prometheus"
)

// 指标默认桶（秒）：60/300/600/1800/3600/7200/14400。
var durationBuckets = []float64{60, 300, 600, 1800, 3600, 7200, 14400}

// Metrics 持有全部 9 个指标的定义。
type Metrics struct {
	BuildResultTotal        *prometheus.CounterVec
	BuildDurationSeconds    *prometheus.HistogramVec
	JobConsecutiveFailures  *prometheus.GaugeVec
	JobLastSuccessTimestamp *prometheus.GaugeVec
	AgentOnline             *prometheus.GaugeVec
	ExecutorBusy            prometheus.Gauge
	QueueLength             prometheus.Gauge
	ExporterScrapeSuccess   prometheus.Gauge
	ExporterLastRefreshTime prometheus.Gauge

	knownResults []string
}

// NewMetrics 创建并注册全部指标到 reg。
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		knownResults: []string{"SUCCESS", "FAILURE", "ABORTED", "UNSTABLE"},
		BuildResultTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "jenkins",
			Name:      "build_result_total",
			Help:      "按 job 与结果累计的构建总数（仅 finished 构建参与累加）。",
		}, []string{"job", "result"}),
		BuildDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "jenkins",
			Name:      "build_duration_seconds",
			Help:      "构建耗时分布（秒）。",
			Buckets:   durationBuckets,
		}, []string{"job"}),
		JobConsecutiveFailures: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "jenkins",
			Name:      "job_consecutive_failures",
			Help:      "job 连续 FAILURE 次数；SUCCESS 清零，UNSTABLE/ABORTED 中性不变。",
		}, []string{"job"}),
		JobLastSuccessTimestamp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "jenkins",
			Name:      "job_last_success_timestamp_seconds",
			Help:      "job 最近一次 SUCCESS 构建时间（秒）。",
		}, []string{"job"}),
		AgentOnline: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "jenkins",
			Name:      "agent_online",
			Help:      "agent 是否在线：1 在线，0 离线（temporarilyOffline 不影响 online 判定）。",
		}, []string{"agent"}),
		ExecutorBusy: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "jenkins",
			Name:      "executor_busy",
			Help:      "全局忙碌 executor 数。",
		}),
		QueueLength: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "jenkins",
			Name:      "queue_length",
			Help:      "Jenkins 构建队列长度。",
		}),
		ExporterScrapeSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "jenkins",
			Name:      "exporter_scrape_success",
			Help:      "自监控：最近一次刷新是否全部成功（1 成功，0 失败）。",
		}),
		ExporterLastRefreshTime: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "jenkins",
			Name:      "exporter_last_refresh_timestamp_seconds",
			Help:      "自监控：最近一次刷新完成时间（秒）。",
		}),
	}
	reg.MustRegister(
		m.BuildResultTotal,
		m.BuildDurationSeconds,
		m.JobConsecutiveFailures,
		m.JobLastSuccessTimestamp,
		m.AgentOnline,
		m.ExecutorBusy,
		m.QueueLength,
		m.ExporterScrapeSuccess,
		m.ExporterLastRefreshTime,
	)
	return m
}

// InitJobSeries 为白名单 job 预建 0 值序列，确保 /metrics 始终输出全部指标族（含 HELP/TYPE）。
func (m *Metrics) InitJobSeries(jobs []string) {
	for _, job := range jobs {
		for _, r := range m.knownResults {
			m.BuildResultTotal.WithLabelValues(job, r)
		}
		m.BuildDurationSeconds.WithLabelValues(job)
		m.JobConsecutiveFailures.WithLabelValues(job).Set(0)
		m.JobLastSuccessTimestamp.WithLabelValues(job).Set(0)
	}
}
