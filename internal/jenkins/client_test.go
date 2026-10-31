package jenkins

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// jobsJSON 构造顶层含一个 folder 与一个叶子 job 的响应。
const jobsJSON = `{"jobs":[
  {"name":"gate-folder","_class":"com.cloudbees.hudson.plugins.folder.Folder","url":"http://x/job/gate-folder"},
  {"name":"ci-gate-pr","_class":"hudson.model.FreeStyleProject","url":"http://x/job/ci-gate-pr"}
]}`

const subJobsJSON = `{"jobs":[
  {"name":"sub-job","_class":"org.jenkinsci.plugins.workflow.job.WorkflowJob","url":"http://x/job/gate-folder/job/sub-job"}
]}`

const buildsJSON = `{"builds":[
  {"number":3,"result":"SUCCESS","duration":120000,"timestamp":1698000000000,"builtOn":"x86-infra-0"},
  {"number":4,"result":null,"duration":0,"timestamp":1698000010000,"builtOn":"x86-infra-1"}
]}`

const computerJSON = `{"computer":[
  {"displayName":"x86-infra-0","offline":false,"temporarilyOffline":false,"numExecutors":4,"busyExecutors":2}
]}`

const queueJSON = `{"items":[{"id":1,"why":"等待","inQueueSince":1698000020000,"task":{"name":"ci-gate-pr"}}]}`

func writeToken(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte("fake-token-for-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// muxHandler 按路径返回不同 JSON，模拟 Jenkins。
func muxHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, jobsJSON)
	})
	mux.HandleFunc("/job/gate-folder/api/json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, subJobsJSON)
	})
	mux.HandleFunc("/job/ci-gate-pr/api/json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, buildsJSON)
	})
	mux.HandleFunc("/computer/api/json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, computerJSON)
	})
	mux.HandleFunc("/queue/api/json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, queueJSON)
	})
	return mux
}

func TestClientParsesAllFourPaths(t *testing.T) {
	srv := httptest.NewServer(muxHandler())
	defer srv.Close()
	c := NewClient(srv.URL, "reader", writeToken(t), 5*time.Second, testLogger())
	ctx := context.Background()

	jobs, err := c.Jobs(ctx, "")
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 2 || jobs[0].Name != "gate-folder" {
		t.Errorf("Jobs = %+v", jobs)
	}

	builds, err := c.Builds(ctx, "ci-gate-pr")
	if err != nil {
		t.Fatalf("Builds: %v", err)
	}
	if len(builds) != 2 {
		t.Fatalf("Builds = %+v", builds)
	}
	if builds[0].Result == nil || *builds[0].Result != "SUCCESS" {
		t.Errorf("build 0 result = %v", builds[0].Result)
	}
	if builds[1].Result != nil {
		t.Errorf("build 1 (running) result 应为 nil")
	}

	comps, err := c.Computers(ctx)
	if err != nil {
		t.Fatalf("Computers: %v", err)
	}
	if len(comps) != 1 || comps[0].DisplayName != "x86-infra-0" {
		t.Errorf("Computers = %+v", comps)
	}

	items, err := c.Queue(ctx)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(items) != 1 || items[0].Task.Name != "ci-gate-pr" {
		t.Errorf("Queue = %+v", items)
	}
}

func TestClientFolderDrillDown(t *testing.T) {
	srv := httptest.NewServer(muxHandler())
	defer srv.Close()
	c := NewClient(srv.URL, "reader", writeToken(t), 5*time.Second, testLogger())

	leaves, err := EnumerateLeafJobs(context.Background(), c, "")
	if err != nil {
		t.Fatalf("EnumerateLeafJobs: %v", err)
	}
	want := map[string]bool{"ci-gate-pr": true, "gate-folder/sub-job": true}
	if len(leaves) != 2 {
		t.Fatalf("leaves = %v", leaves)
	}
	for _, l := range leaves {
		if !want[l] {
			t.Errorf("意外的 leaf %q", l)
		}
	}
}

func TestClientUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "reader", writeToken(t), 5*time.Second, testLogger())
	if _, err := c.Jobs(context.Background(), ""); err != ErrUnauthorized {
		t.Fatalf("应返回 ErrUnauthorized, got %v", err)
	}
}

func TestClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, jobsJSON)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "reader", writeToken(t), 50*time.Millisecond, testLogger())
	_, err := c.Jobs(context.Background(), "")
	if err == nil {
		t.Fatal("超时应返回错误")
	}
}

func TestClientEmptyBuildHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"builds":[]}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "reader", writeToken(t), 5*time.Second, testLogger())
	builds, err := c.Builds(context.Background(), "no-builds-job")
	if err != nil {
		t.Fatalf("Builds: %v", err)
	}
	if len(builds) != 0 {
		t.Errorf("空构建历史应返回 0 条, got %d", len(builds))
	}
}

func TestNewClientDefaultTimeoutWhenZero(t *testing.T) {
	c := NewClient("http://x", "u", writeToken(t), 0, testLogger())
	if c.timeout != 10*time.Second {
		t.Errorf("timeout=0 应默认 10s, got %v", c.timeout)
	}
}

func TestTokenMissingFile(t *testing.T) {
	srv := httptest.NewServer(muxHandler())
	defer srv.Close()
	c := NewClient(srv.URL, "reader", filepath.Join(t.TempDir(), "nope"), 5*time.Second, testLogger())
	if _, err := c.Jobs(context.Background(), ""); err == nil {
		t.Fatal("token 文件缺失应报错")
	}
}

func TestTokenReloadOnMtimeChange(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte("tok1"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClient("http://x", "reader", p, 5*time.Second, testLogger())

	tok1, err := c.Token()
	if err != nil || tok1 != "tok1" {
		t.Fatalf("首次 Token = %q err=%v", tok1, err)
	}
	// 重新读缓存（mtime 未变）
	tok1b, _ := c.Token()
	if tok1b != "tok1" {
		t.Errorf("缓存 token 变化")
	}
	// 改写文件并显式设置不同的 mtime 触发轮转重读。
	if err := os.WriteFile(p, []byte("tok2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, time.Now(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	tok2, err := c.Token()
	if err != nil || tok2 != "tok2" {
		t.Fatalf("轮转后 Token = %q err=%v", tok2, err)
	}
}
