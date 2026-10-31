package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingReturnsBaseline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	st, existed, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if existed {
		t.Errorf("文件不存在 existed 应为 false")
	}
	if st.Version != CurrentVersion {
		t.Errorf("Version = %d", st.Version)
	}
	if st.Get("any") != nil {
		t.Error("空 state 不应有 job")
	}
}

func TestLoadCorruptReturnsBaseline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, existed, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !existed {
		t.Errorf("文件存在 existed 应为 true")
	}
	if st.Get("any") != nil {
		t.Error("损坏 state 应丢弃重建为空")
	}
}

func TestSaveAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	st := New()
	j := st.Ensure("job-a")
	j.LastSeenBuild = 42
	j.ConsecutiveFailures = 3
	j.LastSuccessTimestamp = 1698000000

	if err := st.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp 文件应已被 rename 移除")
	}

	loaded, existed, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !existed {
		t.Error("existed 应为 true")
	}
	got := loaded.Get("job-a")
	if got == nil {
		t.Fatal("job-a 缺失")
	}
	if got.LastSeenBuild != 42 || got.ConsecutiveFailures != 3 || got.LastSuccessTimestamp != 1698000000 {
		t.Errorf("回环值 = %+v", got)
	}
}

func TestSaveCreatesDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "deep", "state.json")
	st := New()
	if err := st.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("文件未创建: %v", err)
	}
}

func TestEnsureIdempotent(t *testing.T) {
	st := New()
	a := st.Ensure("x")
	b := st.Ensure("x")
	if a != b {
		t.Error("Ensure 应返回同一指针")
	}
	a.LastSeenBuild = 9
	if b.LastSeenBuild != 9 {
		t.Error("指针共享失败")
	}
}

func TestLoadReadErrorWhenPathIsDirectory(t *testing.T) {
	// path 指向一个目录，ReadFile 返回非 IsNotExist 错误。
	dir := t.TempDir()
	_, _, err := Load(dir)
	if err == nil {
		t.Fatal("path 为目录应返回读取错误")
	}
}

func TestSaveFailsWhenPathDirUnwritable(t *testing.T) {
	// 把父目录做成一个已存在的文件，MkdirAll 失败。
	dir := t.TempDir()
	blocker := dir + "/block"
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := blocker + "/deep/state.json"
	st := New()
	if err := st.Save(p); err == nil {
		t.Fatal("父路径为文件时 Save 应失败")
	}
}
