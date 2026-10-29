package config

import (
	"testing"
	"time"
)

func setEnvs(t *testing.T, envs map[string]string) {
	t.Helper()
	for k, v := range envs {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{"JENKINS_URL", "JENKINS_USER", "JOB_WHITELIST", "OFFLINE"} {
		t.Setenv(k, "")
	}
	t.Setenv("OFFLINE", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TokenFile != "/vault/secrets/jenkins-token" {
		t.Errorf("TokenFile 默认值 = %q", cfg.TokenFile)
	}
	if cfg.ScrapeInterval != 60*time.Second {
		t.Errorf("ScrapeInterval 默认值 = %v", cfg.ScrapeInterval)
	}
	if cfg.StatePath != "/data/state.json" {
		t.Errorf("StatePath 默认值 = %q", cfg.StatePath)
	}
	if cfg.ListenAddr != ":9300" {
		t.Errorf("ListenAddr 默认值 = %q", cfg.ListenAddr)
	}
	if cfg.HTTPTimeout != 10*time.Second {
		t.Errorf("HTTPTimeout 默认值 = %v", cfg.HTTPTimeout)
	}
	if !cfg.Offline {
		t.Errorf("Offline 应为 true")
	}
}

func TestLoadOnlineRequiresURLAndUser(t *testing.T) {
	t.Setenv("OFFLINE", "false")
	t.Setenv("JENKINS_URL", "")
	t.Setenv("JENKINS_USER", "")
	if _, err := Load(); err == nil {
		t.Fatal("缺少 JENKINS_URL/JENKINS_USER 应报错")
	}
}

func TestLoadOnlineOK(t *testing.T) {
	setEnvs(t, map[string]string{
		"OFFLINE":         "false",
		"JENKINS_URL":     "http://jenkins.jenkins.svc.cluster.local:8080",
		"JENKINS_USER":    "reader",
		"JOB_WHITELIST":   "folder/sub/job1, job2 ,, ",
		"SCRAPE_INTERVAL": "30s",
		"HTTP_TIMEOUT":    "5s",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ScrapeInterval != 30*time.Second {
		t.Errorf("ScrapeInterval = %v", cfg.ScrapeInterval)
	}
	if cfg.HTTPTimeout != 5*time.Second {
		t.Errorf("HTTPTimeout = %v", cfg.HTTPTimeout)
	}
	want := []string{"folder/sub/job1", "job2"}
	if len(cfg.JobWhitelist) != len(want) {
		t.Fatalf("JobWhitelist = %v, want %v", cfg.JobWhitelist, want)
	}
	for i, w := range want {
		if cfg.JobWhitelist[i] != w {
			t.Errorf("JobWhitelist[%d] = %q, want %q", i, cfg.JobWhitelist[i], w)
		}
	}
}

func TestWhitelistMatch(t *testing.T) {
	cfg := Config{JobWhitelist: []string{"folder/sub/job1", "job2"}}
	if !cfg.WhitelistMatch("folder/sub/job1") {
		t.Error("应匹配 folder/sub/job1")
	}
	if !cfg.WhitelistMatch("job2") {
		t.Error("应匹配 job2")
	}
	if cfg.WhitelistMatch("job3") {
		t.Error("不应匹配 job3")
	}
}

func TestLoadInvalidDurations(t *testing.T) {
	t.Setenv("OFFLINE", "false")
	t.Setenv("JENKINS_URL", "http://x")
	t.Setenv("JENKINS_USER", "u")
	t.Setenv("SCRAPE_INTERVAL", "0s")
	if _, err := Load(); err == nil {
		t.Fatal("SCRAPE_INTERVAL=0s 应报错")
	}
}
