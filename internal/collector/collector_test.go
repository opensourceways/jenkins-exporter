package collector

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opensourceways/jenkins-exporter/internal/jenkins"
	"github.com/opensourceways/jenkins-exporter/internal/state"

	obsmetrics "github.com/opensourceways/obs-sdk/go/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// promText 把 registry 渲染成 Prometheus 文本格式。
func promText(t *testing.T, reg prometheus.Gatherer) string {
	t.Helper()
	rw := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rw, httptest.NewRequest("GET", "/metrics", nil))
	return rw.Body.String()
}

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func ptr(s string) *string { return &s }

// mockSource 是可编排的 jenkins.DataSource 桩。
type mockSource struct {
	topJobs       []jenkins.Job
	subJobsByPath map[string][]jenkins.Job
	buildsByJob   map[string][]jenkins.Build
	computers     []jenkins.Computer
	queue         []jenkins.QueueItem
	jobsErr       error
	buildsErr     map[string]error
	computersErr  error
	queueErr      error
	buildsCalls   int
	jobsCalls     int

	folderJobsByPath map[string][]jenkins.Job
	folderJobsErr    map[string]error
}

func (m *mockSource) Jobs(_ context.Context, path string) ([]jenkins.Job, error) {
	m.jobsCalls++
	if m.jobsErr != nil {
		return nil, m.jobsErr
	}
	if path == "" {
		return m.topJobs, nil
	}
	if subs, ok := m.subJobsByPath[path]; ok {
		return subs, nil
	}
	return nil, nil
}

func (m *mockSource) FolderJobs(_ context.Context, path string) ([]jenkins.Job, error) {
	if m.folderJobsErr != nil {
		if e, ok := m.folderJobsErr[path]; ok {
			return nil, e
		}
	}
	return m.folderJobsByPath[path], nil
}

func (m *mockSource) Builds(_ context.Context, job string) ([]jenkins.Build, error) {
	m.buildsCalls++
	if m.buildsErr != nil {
		if e, ok := m.buildsErr[job]; ok {
			return nil, e
		}
	}
	if b, ok := m.buildsByJob[job]; ok {
		return b, nil
	}
	return nil, nil
}

func (m *mockSource) Computers(_ context.Context) ([]jenkins.Computer, error) {
	if m.computersErr != nil {
		return nil, m.computersErr
	}
	return m.computers, nil
}

func (m *mockSource) Queue(_ context.Context) ([]jenkins.QueueItem, error) {
	if m.queueErr != nil {
		return nil, m.queueErr
	}
	return m.queue, nil
}

// testCommunity 是单测里 obs-sdk 的部署级 community 默认值。
const testCommunity = "test"

// newTestMetrics 构造独立的 obs-sdk 指标装配（不注册 go/process 采集器，便于断言）。
func newTestMetrics() *obsmetrics.Metrics {
	noProcess := false
	return obsmetrics.New(obsmetrics.Config{
		Service: "jenkins-exporter", Community: testCommunity, IncludeProcess: &noProcess,
	})
}

func newCollector(t *testing.T, src jenkins.DataSource, whitelist []string, st *state.State) (*Collector, *prometheus.Registry, *Metrics) {
	t.Helper()
	return newCollectorFull(t, src, whitelist, nil, st, 24*time.Hour)
}

// newCollectorFull 允许指定 folder 白名单与停滞阈值。
func newCollectorFull(t *testing.T, src jenkins.DataSource, whitelist, folders []string, st *state.State, stale time.Duration) (*Collector, *prometheus.Registry, *Metrics) {
	t.Helper()
	dir := t.TempDir()
	om := newTestMetrics()
	m := NewMetrics(om)
	if st == nil {
		st = state.New()
	}
	col := NewCollector(src, whitelist, folders, st, dir+"/state.json", m, 60*time.Second, stale, testLog())
	return col, om.Registry(), m
}

func baselineBuilds() []jenkins.Build {
	return []jenkins.Build{
		{Number: 1, Result: ptr("SUCCESS"), Duration: 60000, Timestamp: 1698000000000},
		{Number: 2, Result: ptr("FAILURE"), Duration: 120000, Timestamp: 1698000010000},
		{Number: 3, Result: ptr("FAILURE"), Duration: 90000, Timestamp: 1698000020000},
	}
}

func TestBaselineDoesNotAccumulateCounters(t *testing.T) {
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j1", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j1": baselineBuilds()},
		computers:   []jenkins.Computer{{DisplayName: "a0", Offline: false, BusyExecutors: 2}},
		queue:       []jenkins.QueueItem{{ID: 1}},
	}
	col, _, m := newCollector(t, src, []string{"j1"}, nil)
	ok := col.RefreshOnce(context.Background())
	if !ok {
		t.Fatal("基线刷新应成功")
	}
	if v := testutil.ToFloat64(m.BuildResultTotal.Vec().WithLabelValues(testCommunity, "j1", "FAILURE")); v != 0 {
		t.Errorf("基线不应累加 counter, got %v", v)
	}
	if v := testutil.ToFloat64(m.BuildResultTotal.Vec().WithLabelValues(testCommunity, "j1", "SUCCESS")); v != 0 {
		t.Errorf("基线不应累加 counter, got %v", v)
	}
	if v := testutil.ToFloat64(m.JobConsecutiveFailures.Vec().WithLabelValues(testCommunity, "j1")); v != 0 {
		t.Errorf("基线 consecutive_failures 应为 0, got %v", v)
	}
	// 基线 last_success = 最近 SUCCESS 时间（秒）。
	if v := testutil.ToFloat64(m.JobLastSuccessTimestamp.Vec().WithLabelValues(testCommunity, "j1")); v != 1698000000 {
		t.Errorf("基线 last_success ts = %v, want 1698000000", v)
	}
	st := col.State().Get("j1")
	if st.LastSeenBuild != 3 {
		t.Errorf("基线水位线 = %d, want 3", st.LastSeenBuild)
	}
	// 自监控。
	if v := testutil.ToFloat64(m.ExporterScrapeSuccess.Vec().WithLabelValues(testCommunity)); v != 1 {
		t.Errorf("scrape_success = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.ExecutorBusy.Vec().WithLabelValues(testCommunity)); v != 2 {
		t.Errorf("executor_busy = %v, want 2", v)
	}
	if v := testutil.ToFloat64(m.QueueLength.Vec().WithLabelValues(testCommunity)); v != 1 {
		t.Errorf("queue_length = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.AgentOnline.Vec().WithLabelValues(testCommunity, "a0")); v != 1 {
		t.Errorf("agent_online a0 = %v, want 1", v)
	}
}

func TestIncrementalAccumulatesAndConsecutiveRule(t *testing.T) {
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j1", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j1": baselineBuilds()},
	}
	col, _, m := newCollector(t, src, []string{"j1"}, nil)
	col.RefreshOnce(context.Background())

	// 第二次：追加 F,F,S,F（升序处理），最终 consecutive=1。
	src.buildsByJob["j1"] = append(baselineBuilds(),
		jenkins.Build{Number: 4, Result: ptr("FAILURE"), Duration: 80000, Timestamp: 1698000030000},
		jenkins.Build{Number: 5, Result: ptr("FAILURE"), Duration: 70000, Timestamp: 1698000040000},
		jenkins.Build{Number: 6, Result: ptr("SUCCESS"), Duration: 100000, Timestamp: 1698000050000},
		jenkins.Build{Number: 7, Result: ptr("FAILURE"), Duration: 60000, Timestamp: 1698000060000},
	)
	col.RefreshOnce(context.Background())

	if v := testutil.ToFloat64(m.BuildResultTotal.Vec().WithLabelValues(testCommunity, "j1", "FAILURE")); v != 3 {
		t.Errorf("FAILURE counter = %v, want 3", v)
	}
	if v := testutil.ToFloat64(m.BuildResultTotal.Vec().WithLabelValues(testCommunity, "j1", "SUCCESS")); v != 1 {
		t.Errorf("SUCCESS counter = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.JobConsecutiveFailures.Vec().WithLabelValues(testCommunity, "j1")); v != 1 {
		t.Errorf("consecutive_failures = %v, want 1（末尾 F 在 S 之后）", v)
	}
	if v := testutil.ToFloat64(m.JobLastSuccessTimestamp.Vec().WithLabelValues(testCommunity, "j1")); v != 1698000050 {
		t.Errorf("last_success ts = %v, want 1698000050", v)
	}
	st := col.State().Get("j1")
	if st.LastSeenBuild != 7 {
		t.Errorf("水位线 = %d, want 7", st.LastSeenBuild)
	}
}

func TestConsecutiveNeutralForAbortedUnstable(t *testing.T) {
	// 基线后追加 FAILURE -> 1, ABORTED -> 不变 1, UNSTABLE -> 不变 1。
	builds := []jenkins.Build{
		{Number: 1, Result: ptr("SUCCESS"), Duration: 1000, Timestamp: 1000000},
	}
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j": builds},
	}
	col, _, m := newCollector(t, src, []string{"j"}, nil)
	col.RefreshOnce(context.Background())

	src.buildsByJob["j"] = append(builds,
		jenkins.Build{Number: 2, Result: ptr("FAILURE"), Duration: 1000, Timestamp: 2000000},
		jenkins.Build{Number: 3, Result: ptr("ABORTED"), Duration: 1000, Timestamp: 3000000},
		jenkins.Build{Number: 4, Result: ptr("UNSTABLE"), Duration: 1000, Timestamp: 4000000},
	)
	col.RefreshOnce(context.Background())

	if v := testutil.ToFloat64(m.JobConsecutiveFailures.Vec().WithLabelValues(testCommunity, "j")); v != 1 {
		t.Errorf("ABORTED/UNSTABLE 中性后 consecutive = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.BuildResultTotal.Vec().WithLabelValues(testCommunity, "j", "ABORTED")); v != 1 {
		t.Errorf("ABORTED counter = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.BuildResultTotal.Vec().WithLabelValues(testCommunity, "j", "UNSTABLE")); v != 1 {
		t.Errorf("UNSTABLE counter = %v, want 1", v)
	}
}

func TestUnknownResultAdvancesWaterlineNoCounter(t *testing.T) {
	builds := []jenkins.Build{
		{Number: 1, Result: ptr("SUCCESS"), Duration: 1000, Timestamp: 1000000},
	}
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j": builds},
	}
	col, _, m := newCollector(t, src, []string{"j"}, nil)
	col.RefreshOnce(context.Background())

	src.buildsByJob["j"] = append(builds,
		jenkins.Build{Number: 2, Result: ptr("NOT_BUILT"), Duration: 5000, Timestamp: 2000000},
		jenkins.Build{Number: 3, Result: ptr("FAILURE"), Duration: 5000, Timestamp: 3000000},
	)
	col.RefreshOnce(context.Background())

	// NOT_BUILT 不计入 counter，但水位线推进到 3；FAILURE +1。
	if v := testutil.ToFloat64(m.BuildResultTotal.Vec().WithLabelValues(testCommunity, "j", "FAILURE")); v != 1 {
		t.Errorf("FAILURE counter = %v, want 1", v)
	}
	st := col.State().Get("j")
	if st.LastSeenBuild != 3 {
		t.Errorf("未知 result 应推进水位线 = %d, want 3", st.LastSeenBuild)
	}
}

func TestRunningBuildNotWaterline(t *testing.T) {
	builds := []jenkins.Build{
		{Number: 1, Result: ptr("SUCCESS"), Duration: 1000, Timestamp: 1000000},
	}
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j": builds},
	}
	col, _, _ := newCollector(t, src, []string{"j"}, nil)
	col.RefreshOnce(context.Background())

	// 追加一条 running（result nil, number 5）+ 一条 finished（number 2）。
	src.buildsByJob["j"] = append(builds,
		jenkins.Build{Number: 5, Result: nil, Duration: 0, Timestamp: 5000000},
		jenkins.Build{Number: 2, Result: ptr("SUCCESS"), Duration: 1000, Timestamp: 2000000},
	)
	col.RefreshOnce(context.Background())

	st := col.State().Get("j")
	if st.LastSeenBuild != 2 {
		t.Errorf("running 构建不应推进水位线 = %d, want 2", st.LastSeenBuild)
	}
}

func TestWaterlineNeverRegresses(t *testing.T) {
	// 基线：水位线 = 3。
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j": baselineBuilds()},
	}
	col, _, m := newCollector(t, src, []string{"j"}, nil)
	col.RefreshOnce(context.Background())

	st := col.State().Get("j")
	if st.LastSeenBuild != 3 {
		t.Fatalf("基线水位线 = %d, want 3", st.LastSeenBuild)
	}
	failureBefore := testutil.ToFloat64(m.BuildResultTotal.Vec().WithLabelValues(testCommunity, "j", "FAILURE"))

	// 第二次：Jenkins 返回不全，只回 [1,2]（全部 number ≤ last_seen），模拟构建删除/API 间歇。
	src.buildsByJob["j"] = []jenkins.Build{
		{Number: 1, Result: ptr("SUCCESS"), Duration: 60000, Timestamp: 1698000000000},
		{Number: 2, Result: ptr("FAILURE"), Duration: 120000, Timestamp: 1698000010000},
	}
	col.RefreshOnce(context.Background())

	st = col.State().Get("j")
	if st.LastSeenBuild != 3 {
		t.Errorf("水位线回退 = %d, want 3（水位线只进不退）", st.LastSeenBuild)
	}
	// 已计数过的构建不应重复 Inc。
	failureAfter := testutil.ToFloat64(m.BuildResultTotal.Vec().WithLabelValues(testCommunity, "j", "FAILURE"))
	if failureAfter != failureBefore {
		t.Errorf("回退刷新导致重复计数: FAILURE counter %v -> %v", failureBefore, failureAfter)
	}
}

func TestEmptyBuildHistoryBaseline(t *testing.T) {
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j": {}},
		computers:   []jenkins.Computer{{DisplayName: "a0", Offline: true}},
		queue:       nil,
	}
	col, _, m := newCollector(t, src, []string{"j"}, nil)
	ok := col.RefreshOnce(context.Background())
	if !ok {
		t.Fatal("空构建历史应成功")
	}
	st := col.State().Get("j")
	if st.LastSeenBuild != 0 {
		t.Errorf("空历史水位线 = %d, want 0", st.LastSeenBuild)
	}
	if v := testutil.ToFloat64(m.AgentOnline.Vec().WithLabelValues(testCommunity, "a0")); v != 0 {
		t.Errorf("offline agent 应为 0, got %v", v)
	}
}

func TestJobBuildsFetchFailureSetsScrapeFail(t *testing.T) {
	// 精确白名单走直取，不再拉 job 列表；单个 job 的 builds 拉取失败即为整体失败。
	src := &mockSource{
		topJobs:   []jenkins.Job{{Name: "j", Class: "hudson.model.FreeStyleProject"}},
		buildsErr: map[string]error{"j": jenkins.ErrUnauthorized},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
		queue:     []jenkins.QueueItem{{ID: 1}},
	}
	col, _, m := newCollector(t, src, []string{"j"}, nil)
	ok := col.RefreshOnce(context.Background())
	if ok {
		t.Fatal("job 构建拉取失败应整体失败")
	}
	if v := testutil.ToFloat64(m.ExporterScrapeSuccess.Vec().WithLabelValues(testCommunity)); v != 0 {
		t.Errorf("scrape_success = %v, want 0", v)
	}
}

func TestAllJobsFailSetsScrapeFail(t *testing.T) {
	src := &mockSource{
		topJobs:   []jenkins.Job{{Name: "j", Class: "hudson.model.FreeStyleProject"}},
		buildsErr: map[string]error{"j": jenkins.ErrUnauthorized},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
		queue:     []jenkins.QueueItem{{ID: 1}},
	}
	col, _, m := newCollector(t, src, []string{"j"}, nil)
	ok := col.RefreshOnce(context.Background())
	if ok {
		t.Fatal("全部 job 失败应整体失败")
	}
	if v := testutil.ToFloat64(m.ExporterScrapeSuccess.Vec().WithLabelValues(testCommunity)); v != 0 {
		t.Errorf("scrape_success = %v, want 0", v)
	}
}

func TestOneJobOKOthersFailScrapeSuccess(t *testing.T) {
	src := &mockSource{
		topJobs: []jenkins.Job{
			{Name: "j1", Class: "hudson.model.FreeStyleProject"},
			{Name: "j2", Class: "hudson.model.FreeStyleProject"},
		},
		buildsByJob: map[string][]jenkins.Build{"j1": baselineBuilds()},
		buildsErr:   map[string]error{"j2": jenkins.ErrUnauthorized},
		computers:   []jenkins.Computer{{DisplayName: "a0"}},
		queue:       []jenkins.QueueItem{{ID: 1}},
	}
	col, _, m := newCollector(t, src, []string{"j1", "j2"}, nil)
	ok := col.RefreshOnce(context.Background())
	if !ok {
		t.Fatal("部分 job 成功应整体成功")
	}
	if v := testutil.ToFloat64(m.ExporterScrapeSuccess.Vec().WithLabelValues(testCommunity)); v != 1 {
		t.Errorf("scrape_success = %v, want 1", v)
	}
}

func TestBackoffCappedAtFiveMinutes(t *testing.T) {
	col, _, _ := newCollector(t, &mockSource{}, nil, nil)
	if d := col.backoff(1); d <= 0 || d > maxBackoff {
		t.Errorf("backoff(1) = %v", d)
	}
	if d := col.backoff(30); d > maxBackoff {
		t.Errorf("backoff(30) 应被截断到 5min, got %v", d)
	}
	if d := col.backoff(0); d != 60*time.Second {
		t.Errorf("backoff(0) = %v, want 60s", d)
	}
}

func TestMetricsAllFamiliesExposed(t *testing.T) {
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j1", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j1": baselineBuilds()},
		computers:   []jenkins.Computer{{DisplayName: "a0", Offline: false, BusyExecutors: 1}},
		queue:       []jenkins.QueueItem{{ID: 1}},
	}
	col, reg, _ := newCollector(t, src, []string{"j1"}, nil)
	col.RefreshOnce(context.Background())

	wantFamilies := []string{
		"jenkins_build_result_total",
		"jenkins_build_duration_seconds",
		"jenkins_job_consecutive_failures",
		"jenkins_job_last_success_timestamp_seconds",
		"jenkins_agent_online",
		"jenkins_executor_busy",
		"jenkins_queue_length",
		"jenkins_exporter_scrape_success",
		"jenkins_exporter_last_refresh_timestamp_seconds",
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]bool{}
	for _, mf := range mfs {
		got[mf.GetName()] = true
	}
	for _, w := range wantFamilies {
		if !got[w] {
			t.Errorf("缺少指标族 %q", w)
		}
	}
}

func TestMetricsExposedWithHelpAndType(t *testing.T) {
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j1", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j1": baselineBuilds()},
		computers:   []jenkins.Computer{{DisplayName: "a0", Offline: false, BusyExecutors: 1}},
		queue:       []jenkins.QueueItem{{ID: 1}},
	}
	col, reg, _ := newCollector(t, src, []string{"j1"}, nil)
	col.RefreshOnce(context.Background())

	out, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	// 用 promhttp 序列化验证含 HELP/TYPE 行。
	body := promText(t, reg)
	for _, mf := range out {
		if !strings.Contains(body, "# HELP "+mf.GetName()) {
			t.Errorf("缺少 HELP 行: %s", mf.GetName())
		}
		if !strings.Contains(body, "# TYPE "+mf.GetName()) {
			t.Errorf("缺少 TYPE 行: %s", mf.GetName())
		}
	}
}

func TestMetricsCarryObsLabels(t *testing.T) {
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j1", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j1": baselineBuilds()},
		computers:   []jenkins.Computer{{DisplayName: "a0"}},
		queue:       []jenkins.QueueItem{{ID: 1}},
	}
	col, reg, _ := newCollector(t, src, []string{"j1"}, nil)
	col.RefreshOnce(context.Background())

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	checked := 0
	for _, mf := range mfs {
		if !strings.HasPrefix(mf.GetName(), "jenkins_") {
			continue
		}
		for _, metric := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range metric.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			checked++
			// obs-sdk 自动附加的部署级 label 必须齐全且非空（空值会让采集侧静默漏数）。
			if labels["service"] != "jenkins-exporter" {
				t.Errorf("%s service label = %q", mf.GetName(), labels["service"])
			}
			if labels["community"] != testCommunity {
				t.Errorf("%s community label = %q", mf.GetName(), labels["community"])
			}
			if labels["env"] == "" || labels["instance"] == "" {
				t.Errorf("%s 缺少 env/instance label: %v", mf.GetName(), labels)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有任何 jenkins_* 序列，label 断言形同虚设")
	}
}

func TestMissedBuildsWarn(t *testing.T) {
	// 构造 100 条全部 > last_seen（基线后）。
	var builds []jenkins.Build
	for i := 1; i <= 100; i++ {
		builds = append(builds, jenkins.Build{Number: i, Result: ptr("SUCCESS"), Duration: 1000, Timestamp: int64(i) * 1000})
	}
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j": builds},
	}
	col, _, _ := newCollector(t, src, []string{"j"}, nil)
	// 第一次：基线，水位线=100。
	col.RefreshOnce(context.Background())

	// 第二次：再 100 条 number 101..200，全部新，触发漏计 warn。
	var b2 []jenkins.Build
	for i := 1; i <= 100; i++ {
		b2 = append(b2, jenkins.Build{Number: 100 + i, Result: ptr("SUCCESS"), Duration: 1000, Timestamp: int64(100+i) * 1000})
	}
	src.buildsByJob["j"] = b2
	col.RefreshOnce(context.Background())

	st := col.State().Get("j")
	if st.LastSeenBuild != 200 {
		t.Errorf("水位线 = %d, want 200", st.LastSeenBuild)
	}
}

func TestFolderTraversalWithWhitelist(t *testing.T) {
	src := &mockSource{
		topJobs: []jenkins.Job{
			{Name: "gate", Class: "com.cloudbees.hudson.plugins.folder.Folder"},
			{Name: "flat", Class: "hudson.model.FreeStyleProject"},
		},
		subJobsByPath: map[string][]jenkins.Job{
			"gate": {{Name: "leaf", Class: "hudson.model.FreeStyleProject"}},
		},
		buildsByJob: map[string][]jenkins.Build{"gate/leaf": baselineBuilds()},
		computers:   []jenkins.Computer{{DisplayName: "a0"}},
		queue:       nil,
	}
	col, _, m := newCollector(t, src, []string{"gate/leaf", "missing-job"}, nil)
	ok := col.RefreshOnce(context.Background())
	if !ok {
		t.Fatal("folder 下钻应成功")
	}
	if v := testutil.ToFloat64(m.JobConsecutiveFailures.Vec().WithLabelValues(testCommunity, "gate/leaf")); v != 0 {
		t.Errorf("gate/leaf 基线 consecutive = %v, want 0", v)
	}
}

func TestStatePersistedAcrossCollectorInstances(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/state.json"
	om := newTestMetrics()
	m := NewMetrics(om)
	st := state.New()
	col := NewCollector(&mockSource{
		topJobs:     []jenkins.Job{{Name: "j", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j": baselineBuilds()},
		computers:   []jenkins.Computer{{DisplayName: "a0"}},
		queue:       nil,
	}, []string{"j"}, nil, st, p, m, 60*time.Second, 24*time.Hour, testLog())
	col.RefreshOnce(context.Background())

	// 重新加载 state 文件构造新 collector，水位线应保留。
	loaded, _, err := state.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Get("j").LastSeenBuild != 3 {
		t.Errorf("持久化回环水位线 = %d, want 3", loaded.Get("j").LastSeenBuild)
	}
}

func TestContextCancelledRefreshFails(t *testing.T) {
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j": baselineBuilds()},
		computers:   []jenkins.Computer{{DisplayName: "a0"}},
		queue:       nil,
	}
	col, _, m := newCollector(t, src, []string{"j"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok := col.RefreshOnce(ctx); ok {
		t.Fatal("ctx 取消应失败")
	}
	if v := testutil.ToFloat64(m.ExporterScrapeSuccess.Vec().WithLabelValues(testCommunity)); v != 0 {
		t.Errorf("scrape_success = %v, want 0", v)
	}
}

func summary(n int, result *string, ts int64) *jenkins.BuildSummary {
	return &jenkins.BuildSummary{Number: n, Result: result, Duration: 1000, Timestamp: ts}
}

const (
	testFolder      = "multiarch/src-openeuler/x86-64"
	folderProject   = "hudson.model.FreeStyleProject"
	folderClassPath = "com.cloudbees.hudson.plugins.folder.Folder"
)

func TestFolderAggregation(t *testing.T) {
	now := time.Now()
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{
			testFolder: {
				{Name: "a", Class: folderProject, Color: "blue",
					LastBuild:           summary(3, ptr("SUCCESS"), now.Add(-time.Hour).UnixMilli()),
					LastSuccessfulBuild: summary(3, ptr("SUCCESS"), now.Add(-time.Hour).UnixMilli())},
				{Name: "b", Class: folderProject, Color: "red_anime",
					LastBuild: summary(9, ptr("FAILURE"), now.Add(-2*time.Hour).UnixMilli())},
				{Name: "c", Class: folderProject, Color: "notbuilt"},
				// 曾经成功过、但已超过停滞阈值：计入 stale。
				{Name: "d", Class: folderProject, Color: "blue",
					LastBuild:           summary(2, ptr("SUCCESS"), now.Add(-48*time.Hour).UnixMilli()),
					LastSuccessfulBuild: summary(2, ptr("SUCCESS"), now.Add(-48*time.Hour).UnixMilli())},
				// 子 folder 本身不计入统计。
				{Name: "sub", Class: folderClassPath},
			},
		},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
		queue:     []jenkins.QueueItem{{ID: 1}},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	if ok := col.RefreshOnce(context.Background()); !ok {
		t.Fatal("folder 聚合应成功")
	}

	if v := testutil.ToFloat64(m.FolderJobsTotal.Vec().WithLabelValues(testCommunity, testFolder)); v != 4 {
		t.Errorf("jobs_total = %v, want 4（子 folder 不计入）", v)
	}
	for color, want := range map[string]float64{"blue": 2, "red": 1, "notbuilt": 1, "yellow": 0} {
		if v := testutil.ToFloat64(m.FolderJobsByColor.Vec().WithLabelValues(testCommunity, testFolder, color)); v != want {
			t.Errorf("jobs_by_color{%s} = %v, want %v", color, v, want)
		}
	}
	// 只有 d（曾经构建、但超过 24h）计入停滞；从未构建的 c 不计入。
	if v := testutil.ToFloat64(m.FolderStaleJobs.Vec().WithLabelValues(testCommunity, testFolder)); v != 1 {
		t.Errorf("stale_jobs = %v, want 1", v)
	}
	// folder 内叶子 job 的最近成功时间：有记录取秒值，从未成功为 0。
	if v := testutil.ToFloat64(m.JobLastSuccessTimestamp.Vec().WithLabelValues(testCommunity, testFolder+"/a")); v != float64(now.Add(-time.Hour).UnixMilli()/1000) {
		t.Errorf("a last_success = %v, want ≈now-1h", v)
	}
	if v := testutil.ToFloat64(m.JobLastSuccessTimestamp.Vec().WithLabelValues(testCommunity, testFolder+"/c")); v != 0 {
		t.Errorf("c 从未成功应为 0, got %v", v)
	}
	// 最近构建是 a 的 1 小时前。
	age := testutil.ToFloat64(m.FolderLastBuildAge.Vec().WithLabelValues(testCommunity, testFolder))
	if age < 3500 || age > 3700 {
		t.Errorf("last_build_age = %v, want ≈3600", age)
	}
}

func TestFolderStaleByThreshold(t *testing.T) {
	now := time.Now()
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{
			testFolder: {
				// 25 小时前构建，超过 24h 阈值 → 停滞。
				{Name: "old", Class: folderProject, Color: "blue",
					LastBuild: summary(1, ptr("SUCCESS"), now.Add(-25*time.Hour).UnixMilli())},
			},
		},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())

	if v := testutil.ToFloat64(m.FolderStaleJobs.Vec().WithLabelValues(testCommunity, testFolder)); v != 1 {
		t.Errorf("stale_jobs = %v, want 1", v)
	}
}

func TestFolderEmptyAndNoBuildAge(t *testing.T) {
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {}},
		computers:        []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	if ok := col.RefreshOnce(context.Background()); !ok {
		t.Fatal("空 folder 不应失败")
	}
	if v := testutil.ToFloat64(m.FolderJobsTotal.Vec().WithLabelValues(testCommunity, testFolder)); v != 0 {
		t.Errorf("jobs_total = %v, want 0", v)
	}
	// 无任何构建时年龄固定为 0，避免大盘出现无意义的大数。
	if v := testutil.ToFloat64(m.FolderLastBuildAge.Vec().WithLabelValues(testCommunity, testFolder)); v != 0 {
		t.Errorf("last_build_age = %v, want 0", v)
	}
}

func TestFolderLastSuccessAgeDistinguishesFromLastBuild(t *testing.T) {
	now := time.Now()
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			// 最近一次构建 1 分钟前失败，最近一次成功 3 小时前：
			// last_build_age 很小（看似健康），last_success_age 很大（真实健康度差）。
			{Name: "flaky", Class: folderProject, Color: "red",
				LastBuild:           summary(10, ptr("FAILURE"), now.Add(-time.Minute).UnixMilli()),
				LastSuccessfulBuild: summary(9, ptr("SUCCESS"), now.Add(-3*time.Hour).UnixMilli())},
			{Name: "never", Class: folderProject, Color: "notbuilt"},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())

	buildAge := testutil.ToFloat64(m.FolderLastBuildAge.Vec().WithLabelValues(testCommunity, testFolder))
	if buildAge < 50 || buildAge > 70 {
		t.Errorf("last_build_age = %v, want ≈60", buildAge)
	}
	successAge := testutil.ToFloat64(m.FolderLastSuccessAge.Vec().WithLabelValues(testCommunity, testFolder))
	if successAge < 3*3600-10 || successAge > 3*3600+10 {
		t.Errorf("last_success_age = %v, want ≈10800", successAge)
	}
}

func TestQueueOldestWaitAndExecutorTotal(t *testing.T) {
	now := time.Now()
	src := &mockSource{
		computers: []jenkins.Computer{
			{DisplayName: "a0", NumExecutors: 4, BusyExecutors: 3},
			{DisplayName: "a1", NumExecutors: 2, BusyExecutors: 1},
		},
		queue: []jenkins.QueueItem{
			{ID: 1, InQueueSince: now.Add(-30 * time.Second).UnixMilli()},
			{ID: 2, InQueueSince: now.Add(-10 * time.Minute).UnixMilli()},
		},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())

	if v := testutil.ToFloat64(m.ExecutorTotal.Vec().WithLabelValues(testCommunity)); v != 6 {
		t.Errorf("executor_total = %v, want 6", v)
	}
	if v := testutil.ToFloat64(m.ExecutorBusy.Vec().WithLabelValues(testCommunity)); v != 4 {
		t.Errorf("executor_busy = %v, want 4", v)
	}
	// 取最早入队项（10 分钟前），而不是最近入队项。
	wait := testutil.ToFloat64(m.QueueOldestWait.Vec().WithLabelValues(testCommunity))
	if wait < 590 || wait > 610 {
		t.Errorf("queue_oldest_wait = %v, want ≈600", wait)
	}
}

func TestQueueOldestWaitEmptyQueue(t *testing.T) {
	src := &mockSource{computers: []jenkins.Computer{{DisplayName: "a0"}}}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())
	if v := testutil.ToFloat64(m.QueueOldestWait.Vec().WithLabelValues(testCommunity)); v != 0 {
		t.Errorf("空队列 oldest_wait = %v, want 0", v)
	}
}

// newCollectorWithLog 构造可捕获告警日志的 collector（folder 模式）。
func newCollectorWithLog(t *testing.T, src jenkins.DataSource, folders []string, log *slog.Logger) *Collector {
	t.Helper()
	om := newTestMetrics()
	m := NewMetrics(om)
	return NewCollector(src, nil, folders, state.New(), t.TempDir()+"/state.json", m,
		60*time.Second, 24*time.Hour, log)
}

func warnBuffer() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})), buf
}

func TestRedAlertOnlyAfterTwoConsecutiveRounds(t *testing.T) {
	log, buf := warnBuffer()
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "broken", Class: folderProject, Color: "red"},
			{Name: "ok", Class: folderProject, Color: "blue"},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col := newCollectorWithLog(t, src, []string{testFolder}, log)

	// 第 1 轮：首次变红，不告警（可能是正在进行的偶发失败）。
	col.RefreshOnce(context.Background())
	if strings.Contains(buf.String(), "连续两轮失败") {
		t.Fatalf("第 1 轮不应告警，实际输出: %s", buf.String())
	}

	// 第 2 轮：连续两轮 red，告警且带 job 全路径。
	buf.Reset()
	col.RefreshOnce(context.Background())
	if !strings.Contains(buf.String(), "连续两轮失败") || !strings.Contains(buf.String(), testFolder+"/broken") {
		t.Fatalf("第 2 轮应告警并带 job 路径，实际输出: %s", buf.String())
	}
	// 正常的 blue job 不应出现。
	if strings.Contains(buf.String(), testFolder+"/ok") {
		t.Errorf("blue job 不应被告警: %s", buf.String())
	}

	// 第 3 轮：同一故障不重复告警。
	buf.Reset()
	col.RefreshOnce(context.Background())
	if strings.Contains(buf.String(), "连续两轮失败") {
		t.Fatalf("同一故障不应重复告警，实际输出: %s", buf.String())
	}
}

func TestRedAlertRearmsAfterRecovery(t *testing.T) {
	log, buf := warnBuffer()
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "flaky", Class: folderProject, Color: "red"},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col := newCollectorWithLog(t, src, []string{testFolder}, log)

	col.RefreshOnce(context.Background()) // 第 1 轮 red
	col.RefreshOnce(context.Background()) // 第 2 轮 red -> 告警
	buf.Reset()

	// 恢复为绿。
	src.folderJobsByPath[testFolder][0].Color = "blue"
	col.RefreshOnce(context.Background())
	if strings.Contains(buf.String(), "连续两轮失败") {
		t.Errorf("恢复为绿时不应告警: %s", buf.String())
	}

	// 再次变红：第 1 轮不告警。
	src.folderJobsByPath[testFolder][0].Color = "red"
	buf.Reset()
	col.RefreshOnce(context.Background())
	if strings.Contains(buf.String(), "连续两轮失败") {
		t.Errorf("重新变红的第 1 轮不应告警: %s", buf.String())
	}

	// 第 2 轮：重新告警。
	buf.Reset()
	col.RefreshOnce(context.Background())
	if !strings.Contains(buf.String(), "连续两轮失败") {
		t.Errorf("重新变红第 2 轮应再次告警: %s", buf.String())
	}
}

func TestRedStatePersistsAcrossRestart(t *testing.T) {
	log, buf := warnBuffer()
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "broken", Class: folderProject, Color: "red"},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	dir := t.TempDir()
	statePath := dir + "/state.json"
	build := func() *Collector {
		return NewCollector(src, nil, []string{testFolder}, state.New(), statePath,
			NewMetrics(newTestMetrics()), 60*time.Second, 24*time.Hour, log)
	}
	col := build()
	col.RefreshOnce(context.Background())
	col.RefreshOnce(context.Background()) // 告警
	if !strings.Contains(buf.String(), "连续两轮失败") {
		t.Fatalf("应告警，实际输出: %s", buf.String())
	}

	// 模拟重启：从同一 state 文件重新加载，red 记录不应丢失，避免重复告警。
	saved, existed, err := state.Load(statePath)
	if err != nil || !existed {
		t.Fatalf("加载 state 失败: err=%v existed=%v", err, existed)
	}
	if len(saved.RedSeen) != 1 || len(saved.Alerted) != 1 {
		t.Fatalf("red 状态未持久化: seen=%v alerted=%v", saved.RedSeen, saved.Alerted)
	}
	buf.Reset()
	restarted := NewCollector(src, nil, []string{testFolder}, saved, statePath,
		NewMetrics(newTestMetrics()), 60*time.Second, 24*time.Hour, log)
	restarted.RefreshOnce(context.Background())
	if strings.Contains(buf.String(), "连续两轮失败") {
		t.Errorf("重启后不应重复告警: %s", buf.String())
	}
}

func TestRedAlertLimitedPerRound(t *testing.T) {
	log, buf := warnBuffer()
	jobs := make([]jenkins.Job, 0, maxRedAlertsPerRound+10)
	for i := 0; i < maxRedAlertsPerRound+10; i++ {
		jobs = append(jobs, jenkins.Job{Name: fmt.Sprintf("j%03d", i), Class: folderProject, Color: "red"})
	}
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: jobs},
		computers:        []jenkins.Computer{{DisplayName: "a0"}},
	}
	col := newCollectorWithLog(t, src, []string{testFolder}, log)
	col.RefreshOnce(context.Background())
	buf.Reset()
	col.RefreshOnce(context.Background())

	lines := strings.Count(buf.String(), "连续两轮失败")
	if lines != maxRedAlertsPerRound {
		t.Errorf("单轮告警条数 = %d, want %d", lines, maxRedAlertsPerRound)
	}
	if !strings.Contains(buf.String(), "已省略") {
		t.Errorf("超出上限应输出省略汇总: %s", buf.String())
	}
}

func TestRedStateNotAdvancedWhenFolderFetchFails(t *testing.T) {
	log, buf := warnBuffer()
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "broken", Class: folderProject, Color: "red"},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col := newCollectorWithLog(t, src, []string{testFolder}, log)
	col.RefreshOnce(context.Background()) // 第 1 轮：建立 redSeen

	// 第 2 轮拉取失败：不能把 red 当作已恢复而清空。
	src.folderJobsErr = map[string]error{testFolder: jenkins.ErrUnauthorized}
	col.RefreshOnce(context.Background())
	if len(col.State().RedSeen) != 1 {
		t.Errorf("抓取失败不应清空 red 状态, got %v", col.State().RedSeen)
	}

	// 第 3 轮恢复：仍为 red，视为连续，应告警。
	src.folderJobsErr = nil
	buf.Reset()
	col.RefreshOnce(context.Background())
	if !strings.Contains(buf.String(), "连续两轮失败") {
		t.Errorf("跨失败轮次后仍应告警: %s", buf.String())
	}
}

func TestFolderFetchFailureSetsScrapeFail(t *testing.T) {
	src := &mockSource{
		folderJobsErr: map[string]error{testFolder: jenkins.ErrUnauthorized},
		computers:     []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	if ok := col.RefreshOnce(context.Background()); ok {
		t.Fatal("folder 拉取失败应整体失败")
	}
	if v := testutil.ToFloat64(m.ExporterScrapeSuccess.Vec().WithLabelValues(testCommunity)); v != 0 {
		t.Errorf("scrape_success = %v, want 0", v)
	}
}

func TestJobAndFolderWhitelistCoexist(t *testing.T) {
	now := time.Now()
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "j", Class: folderProject}},
		buildsByJob: map[string][]jenkins.Build{"j": baselineBuilds()},
		folderJobsByPath: map[string][]jenkins.Job{
			testFolder: {
				{Name: "a", Class: folderProject, Color: "blue",
					LastBuild: summary(1, ptr("SUCCESS"), now.Add(-time.Minute).UnixMilli())},
			},
		},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
		queue:     []jenkins.QueueItem{{ID: 1}},
	}
	col, _, m := newCollectorFull(t, src, []string{"j"}, []string{testFolder}, nil, 24*time.Hour)
	if ok := col.RefreshOnce(context.Background()); !ok {
		t.Fatal("两条路径共存应成功")
	}
	if v := testutil.ToFloat64(m.JobConsecutiveFailures.Vec().WithLabelValues(testCommunity, "j")); v != 0 {
		t.Errorf("精确 job 指标缺失, consecutive = %v", v)
	}
	if v := testutil.ToFloat64(m.FolderJobsTotal.Vec().WithLabelValues(testCommunity, testFolder)); v != 1 {
		t.Errorf("folder 指标缺失, jobs_total = %v", v)
	}
}

func TestFolderMetricsFamiliesExposed(t *testing.T) {
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "a", Class: folderProject, Color: "blue"},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, reg, _ := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())

	body := promText(t, reg)
	for _, name := range []string{
		"jenkins_folder_jobs_total",
		"jenkins_folder_jobs_by_color",
		"jenkins_folder_last_build_age_seconds",
		"jenkins_folder_last_success_age_seconds",
		"jenkins_folder_stale_jobs",
	} {
		if !strings.Contains(body, "# HELP "+name) || !strings.Contains(body, "# TYPE "+name) {
			t.Errorf("缺少指标族 %q 的 HELP/TYPE", name)
		}
	}
}

func TestNormalizeColor(t *testing.T) {
	cases := map[string]string{
		"blue":       "blue",
		"blue_anime": "blue",
		"red_anime":  "red",
		"yellow":     "yellow",
		"aborted":    "aborted",
		"notbuilt":   "notbuilt",
		"disabled":   "disabled",
		"grey":       "unknown",
		"":           "unknown",
	}
	for in, want := range cases {
		if got := normalizeColor(in); got != want {
			t.Errorf("normalizeColor(%q) = %q, want %q", in, got, want)
		}
	}
}

// 精确白名单必须直取：任何一次 Jobs() 调用都意味着又在递归遍历整棵树。
func TestJobWhitelistFetchesDirectlyWithoutEnumeration(t *testing.T) {
	const deep = "multiarch/openeuler/x86-64/Gluten"
	src := &mockSource{
		topJobs:     []jenkins.Job{{Name: "gate", Class: folderClassPath}},
		buildsByJob: map[string][]jenkins.Build{deep: baselineBuilds()},
		computers:   []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, _, m := newCollector(t, src, []string{deep}, nil)
	if ok := col.RefreshOnce(context.Background()); !ok {
		t.Fatal("精确白名单直取应成功")
	}
	if src.jobsCalls != 0 {
		t.Errorf("不应调用 Jobs() 枚举, 实际调用 %d 次", src.jobsCalls)
	}
	if src.buildsCalls != 1 {
		t.Errorf("builds 调用 = %d, want 1（每个白名单 job 一次）", src.buildsCalls)
	}
	if v := testutil.ToFloat64(m.JobConsecutiveFailures.Vec().WithLabelValues(testCommunity, deep)); v != 0 {
		t.Errorf("consecutive_failures = %v, want 0", v)
	}
}

// 失败 job 的身份指标：只有 red 的 job 出序列，blue 的 job 不出。
func TestJobRedSeriesMarksOnlyFailedJobs(t *testing.T) {
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "broken", Class: folderProject, Color: "red_anime"},
			{Name: "ok", Class: folderProject, Color: "blue"},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, reg, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())

	if v := testutil.ToFloat64(m.JobRed.Vec().WithLabelValues(testCommunity, testFolder+"/broken")); v != 1 {
		t.Errorf("red job 序列 = %v, want 1", v)
	}
	body := promText(t, reg)
	var redBroken, redOK bool
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "jenkins_job_red{") {
			continue
		}
		if strings.Contains(line, `job="`+testFolder+`/broken"`) {
			redBroken = true
		}
		if strings.Contains(line, `job="`+testFolder+`/ok"`) {
			redOK = true
		}
	}
	if !redBroken {
		t.Errorf("缺少 red job 序列: %s", body)
	}
	if redOK {
		t.Errorf("blue job 不应产生 jenkins_job_red 序列: %s", body)
	}
}

func TestJobRedSeriesClearsAfterRecovery(t *testing.T) {
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "flaky", Class: folderProject, Color: "red"},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())
	if v := testutil.ToFloat64(m.JobRed.Vec().WithLabelValues(testCommunity, testFolder+"/flaky")); v != 1 {
		t.Fatalf("red 状态 = %v, want 1", v)
	}

	src.folderJobsByPath[testFolder][0].Color = "blue"
	col.RefreshOnce(context.Background())
	if v := testutil.ToFloat64(m.JobRed.Vec().WithLabelValues(testCommunity, testFolder+"/flaky")); v != 0 {
		t.Errorf("恢复后应置 0, got %v", v)
	}
}

func TestJobRedSeriesKeptWhenFolderFetchFails(t *testing.T) {
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "broken", Class: folderProject, Color: "red"},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())

	// 拉取失败时不能把指标误判为已恢复。
	src.folderJobsErr = map[string]error{testFolder: jenkins.ErrUnauthorized}
	col.RefreshOnce(context.Background())
	if v := testutil.ToFloat64(m.JobRed.Vec().WithLabelValues(testCommunity, testFolder+"/broken")); v != 1 {
		t.Errorf("抓取失败后应保持 1, got %v", v)
	}
}

// 失败 job 的构建号：值取 lastBuild.number，job_url 为恒定的 job 页面地址，
// 大盘用 job_url + 值拼出构建日志页。
func TestJobLastFailedBuildAndURL(t *testing.T) {
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "broken", Class: folderProject, Color: "red_anime", LastBuild: &jenkins.BuildSummary{Number: 1221}},
			{Name: "ok", Class: folderProject, Color: "blue"},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	log, _ := warnBuffer()
	om := newTestMetrics()
	m := NewMetrics(om)
	col := NewCollector(src, nil, []string{testFolder}, state.New(), t.TempDir()+"/state.json", m,
		60*time.Second, 24*time.Hour, log, WithWebURL("https://ci.example.com/"))
	col.RefreshOnce(context.Background())

	job := testFolder + "/broken"
	wantURL := "https://ci.example.com/job/" + strings.ReplaceAll(job, "/", "/job/")
	if v := testutil.ToFloat64(m.JobLastFailedBuild.Vec().WithLabelValues(testCommunity, job, wantURL)); v != 1221 {
		t.Errorf("失败构建号 = %v, want 1221", v)
	}

	// 恢复正常后构建号应清零，避免大盘继续展示已修复的历史失败。
	src.folderJobsByPath[testFolder][0].Color = "blue"
	col.RefreshOnce(context.Background())
	if v := testutil.ToFloat64(m.JobLastFailedBuild.Vec().WithLabelValues(testCommunity, job, wantURL)); v != 0 {
		t.Errorf("恢复后构建号应置 0, got %v", v)
	}
}

// 未配置 webURL 时链接标签为空，但不能影响指标数值。
func TestJobLastFailedBuildWithoutWebURL(t *testing.T) {
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "broken", Class: folderProject, Color: "red", LastBuild: &jenkins.BuildSummary{Number: 7}},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	log, _ := warnBuffer()
	m := NewMetrics(newTestMetrics())
	col := NewCollector(src, nil, []string{testFolder}, state.New(), t.TempDir()+"/state.json", m,
		60*time.Second, 24*time.Hour, log)
	col.RefreshOnce(context.Background())

	if v := testutil.ToFloat64(m.JobLastFailedBuild.Vec().WithLabelValues(testCommunity, testFolder+"/broken", "")); v != 7 {
		t.Errorf("失败构建号 = %v, want 7", v)
	}
}

// 每个叶子 job 的最近构建时间：Jenkins 毫秒转秒，从未构建为 0；
// job 从 folder 移除后同样置 0，避免陈旧时间戳长期触发「超过一天没有执行」。
func TestJobLastBuildTimestamp(t *testing.T) {
	ts := time.Now().Add(-30 * time.Hour).UnixMilli()
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "old", Class: folderProject, Color: "blue", LastBuild: summary(5, ptr("SUCCESS"), ts)},
			{Name: "never", Class: folderProject, Color: "notbuilt"},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())

	if v := testutil.ToFloat64(m.JobLastBuildTimestamp.Vec().WithLabelValues(testCommunity, testFolder+"/old")); v != float64(ts/1000) {
		t.Errorf("last_build = %v, want %v（秒）", v, float64(ts/1000))
	}
	if v := testutil.ToFloat64(m.JobLastBuildTimestamp.Vec().WithLabelValues(testCommunity, testFolder+"/never")); v != 0 {
		t.Errorf("从未构建应为 0, got %v", v)
	}

	// 移除 job 后下一轮应置 0。
	src.folderJobsByPath[testFolder] = src.folderJobsByPath[testFolder][1:]
	col.RefreshOnce(context.Background())
	if v := testutil.ToFloat64(m.JobLastBuildTimestamp.Vec().WithLabelValues(testCommunity, testFolder+"/old")); v != 0 {
		t.Errorf("job 移除后应置 0, got %v", v)
	}
}

// folder 拉取失败时保留上一轮时间戳，不能误判为「从未构建」。
func TestJobLastBuildTimestampKeptWhenFolderFetchFails(t *testing.T) {
	ts := time.Now().Add(-time.Hour).UnixMilli()
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "a", Class: folderProject, Color: "blue", LastBuild: summary(1, ptr("SUCCESS"), ts)},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())

	src.folderJobsErr = map[string]error{testFolder: jenkins.ErrUnauthorized}
	col.RefreshOnce(context.Background())
	if v := testutil.ToFloat64(m.JobLastBuildTimestamp.Vec().WithLabelValues(testCommunity, testFolder+"/a")); v != float64(ts/1000) {
		t.Errorf("抓取失败后时间戳应保持不变, got %v", v)
	}
}

// 红色 folder job 的连续失败次数：从最新构建往回数，SUCCESS 终止。
func TestRedJobConsecutiveFailures(t *testing.T) {
	job := testFolder + "/broken"
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "broken", Class: folderProject, Color: "red", LastBuild: summary(5, ptr("FAILURE"), 1698000050000)},
		}},
		buildsByJob: map[string][]jenkins.Build{job: {
			{Number: 2, Result: ptr("SUCCESS")},
			{Number: 3, Result: ptr("FAILURE")},
			{Number: 4, Result: ptr("FAILURE")},
			{Number: 5, Result: ptr("FAILURE")},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())

	if v := testutil.ToFloat64(m.JobConsecutiveFailures.Vec().WithLabelValues(testCommunity, job)); v != 3 {
		t.Errorf("连续失败次数 = %v, want 3", v)
	}
}

// UNSTABLE/ABORTED 视为中性：不累加也不终止连续失败计数。
func TestRedJobConsecutiveFailuresNeutralResults(t *testing.T) {
	job := testFolder + "/flaky"
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "flaky", Class: folderProject, Color: "red", LastBuild: summary(4, ptr("FAILURE"), 1698000040000)},
		}},
		buildsByJob: map[string][]jenkins.Build{job: {
			{Number: 1, Result: ptr("SUCCESS")},
			{Number: 2, Result: ptr("FAILURE")},
			{Number: 3, Result: ptr("ABORTED")},
			{Number: 4, Result: ptr("FAILURE")},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())

	if v := testutil.ToFloat64(m.JobConsecutiveFailures.Vec().WithLabelValues(testCommunity, job)); v != 2 {
		t.Errorf("连续失败次数 = %v, want 2（ABORTED 中性跳过）", v)
	}
}

// 最近构建号没变时复用缓存，不重复回源拉构建历史。
func TestRedJobStreakCachedByLastBuildNumber(t *testing.T) {
	job := testFolder + "/broken"
	src := &mockSource{
		folderJobsByPath: map[string][]jenkins.Job{testFolder: {
			{Name: "broken", Class: folderProject, Color: "red", LastBuild: summary(2, ptr("FAILURE"), 1698000020000)},
		}},
		buildsByJob: map[string][]jenkins.Build{job: {
			{Number: 1, Result: ptr("SUCCESS")},
			{Number: 2, Result: ptr("FAILURE")},
		}},
		computers: []jenkins.Computer{{DisplayName: "a0"}},
	}
	col, _, m := newCollectorFull(t, src, nil, []string{testFolder}, nil, 24*time.Hour)
	col.RefreshOnce(context.Background())
	if src.buildsCalls != 1 {
		t.Fatalf("首轮应回源 1 次, got %d", src.buildsCalls)
	}

	col.RefreshOnce(context.Background())
	if src.buildsCalls != 1 {
		t.Errorf("构建号未变不应重复回源, got %d", src.buildsCalls)
	}
	if v := testutil.ToFloat64(m.JobConsecutiveFailures.Vec().WithLabelValues(testCommunity, job)); v != 1 {
		t.Errorf("缓存值应继续输出 = 1, got %v", v)
	}

	// 新构建出现：重新回源并更新。
	src.folderJobsByPath[testFolder][0].LastBuild = summary(3, ptr("FAILURE"), 1698000030000)
	src.buildsByJob[job] = append(src.buildsByJob[job], jenkins.Build{Number: 3, Result: ptr("FAILURE")})
	col.RefreshOnce(context.Background())
	if src.buildsCalls != 2 {
		t.Errorf("新构建应触发回源, got %d", src.buildsCalls)
	}
	if v := testutil.ToFloat64(m.JobConsecutiveFailures.Vec().WithLabelValues(testCommunity, job)); v != 2 {
		t.Errorf("连续失败次数 = %v, want 2", v)
	}
}
