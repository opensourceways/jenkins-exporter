package collector

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opensourceways/jenkins-exporter/internal/jenkins"
	"github.com/opensourceways/jenkins-exporter/internal/state"
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
}

func (m *mockSource) Jobs(_ context.Context, path string) ([]jenkins.Job, error) {
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

func newCollector(t *testing.T, src jenkins.DataSource, whitelist []string, st *state.State) (*Collector, *prometheus.Registry, *Metrics) {
	t.Helper()
	dir := t.TempDir()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	if st == nil {
		st = state.New()
	}
	col := NewCollector(src, whitelist, st, dir+"/state.json", m, 60*time.Second, testLog())
	return col, reg, m
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
	if v := testutil.ToFloat64(m.BuildResultTotal.WithLabelValues("j1", "FAILURE")); v != 0 {
		t.Errorf("基线不应累加 counter, got %v", v)
	}
	if v := testutil.ToFloat64(m.BuildResultTotal.WithLabelValues("j1", "SUCCESS")); v != 0 {
		t.Errorf("基线不应累加 counter, got %v", v)
	}
	if v := testutil.ToFloat64(m.JobConsecutiveFailures.WithLabelValues("j1")); v != 0 {
		t.Errorf("基线 consecutive_failures 应为 0, got %v", v)
	}
	// 基线 last_success = 最近 SUCCESS 时间（秒）。
	if v := testutil.ToFloat64(m.JobLastSuccessTimestamp.WithLabelValues("j1")); v != 1698000000 {
		t.Errorf("基线 last_success ts = %v, want 1698000000", v)
	}
	st := col.State().Get("j1")
	if st.LastSeenBuild != 3 {
		t.Errorf("基线水位线 = %d, want 3", st.LastSeenBuild)
	}
	// 自监控。
	if v := testutil.ToFloat64(m.ExporterScrapeSuccess); v != 1 {
		t.Errorf("scrape_success = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.ExecutorBusy); v != 2 {
		t.Errorf("executor_busy = %v, want 2", v)
	}
	if v := testutil.ToFloat64(m.QueueLength); v != 1 {
		t.Errorf("queue_length = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.AgentOnline.WithLabelValues("a0")); v != 1 {
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

	if v := testutil.ToFloat64(m.BuildResultTotal.WithLabelValues("j1", "FAILURE")); v != 3 {
		t.Errorf("FAILURE counter = %v, want 3", v)
	}
	if v := testutil.ToFloat64(m.BuildResultTotal.WithLabelValues("j1", "SUCCESS")); v != 1 {
		t.Errorf("SUCCESS counter = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.JobConsecutiveFailures.WithLabelValues("j1")); v != 1 {
		t.Errorf("consecutive_failures = %v, want 1（末尾 F 在 S 之后）", v)
	}
	if v := testutil.ToFloat64(m.JobLastSuccessTimestamp.WithLabelValues("j1")); v != 1698000050 {
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

	if v := testutil.ToFloat64(m.JobConsecutiveFailures.WithLabelValues("j")); v != 1 {
		t.Errorf("ABORTED/UNSTABLE 中性后 consecutive = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.BuildResultTotal.WithLabelValues("j", "ABORTED")); v != 1 {
		t.Errorf("ABORTED counter = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.BuildResultTotal.WithLabelValues("j", "UNSTABLE")); v != 1 {
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
	if v := testutil.ToFloat64(m.BuildResultTotal.WithLabelValues("j", "FAILURE")); v != 1 {
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
	failureBefore := testutil.ToFloat64(m.BuildResultTotal.WithLabelValues("j", "FAILURE"))

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
	failureAfter := testutil.ToFloat64(m.BuildResultTotal.WithLabelValues("j", "FAILURE"))
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
	if v := testutil.ToFloat64(m.AgentOnline.WithLabelValues("a0")); v != 0 {
		t.Errorf("offline agent 应为 0, got %v", v)
	}
}

func TestJobsFetchFailureSetsScrapeFail(t *testing.T) {
	src := &mockSource{
		jobsErr:   jenkins.ErrUnauthorized,
		computers: []jenkins.Computer{{DisplayName: "a0"}},
		queue:     []jenkins.QueueItem{{ID: 1}},
	}
	col, _, m := newCollector(t, src, []string{"j"}, nil)
	ok := col.RefreshOnce(context.Background())
	if ok {
		t.Fatal("job 列表失败应整体失败")
	}
	if v := testutil.ToFloat64(m.ExporterScrapeSuccess); v != 0 {
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
	if v := testutil.ToFloat64(m.ExporterScrapeSuccess); v != 0 {
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
	if v := testutil.ToFloat64(m.ExporterScrapeSuccess); v != 1 {
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
	if v := testutil.ToFloat64(m.JobConsecutiveFailures.WithLabelValues("gate/leaf")); v != 0 {
		t.Errorf("gate/leaf 基线 consecutive = %v, want 0", v)
	}
}

func TestStatePersistedAcrossCollectorInstances(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/state.json"
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	st := state.New()
	col := NewCollector(&mockSource{
		topJobs:     []jenkins.Job{{Name: "j", Class: "hudson.model.FreeStyleProject"}},
		buildsByJob: map[string][]jenkins.Build{"j": baselineBuilds()},
		computers:   []jenkins.Computer{{DisplayName: "a0"}},
		queue:       nil,
	}, []string{"j"}, st, p, m, 60*time.Second, testLog())
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
	if v := testutil.ToFloat64(m.ExporterScrapeSuccess); v != 0 {
		t.Errorf("scrape_success = %v, want 0", v)
	}
}
