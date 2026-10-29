package offline

import (
	"context"
	"path/filepath"
	"testing"
)

// 使用仓库内置 testdata 验证离线数据源能解析四类 fixture。
func TestSourceReadsBuiltinFixtures(t *testing.T) {
	dir, _ := filepath.Abs("../../testdata")
	src := New(dir)
	ctx := context.Background()

	jobs, err := src.Jobs(ctx, "")
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) == 0 {
		t.Fatal("jobs.json 应非空")
	}
	// 子路径返回空（离线不模拟 folder 下钻）。
	if subs, _ := src.Jobs(ctx, "any"); len(subs) != 0 {
		t.Errorf("离线子路径应返回空")
	}

	builds, err := src.Builds(ctx, "ci-gate-pr")
	if err != nil {
		t.Fatalf("Builds: %v", err)
	}
	if len(builds) == 0 {
		t.Fatal("builds.json 应非空")
	}

	comps, err := src.Computers(ctx)
	if err != nil {
		t.Fatalf("Computers: %v", err)
	}
	if len(comps) == 0 {
		t.Fatal("computer.json 应非空")
	}

	items, err := src.Queue(ctx)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("queue.json 应非空")
	}
}

func TestSourceMissingFileErrors(t *testing.T) {
	src := New(t.TempDir())
	if _, err := src.Jobs(context.Background(), ""); err == nil {
		t.Fatal("缺失 fixture 应报错")
	}
}
