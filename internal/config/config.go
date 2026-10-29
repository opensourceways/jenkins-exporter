// Package config 加载 exporter 配置（环境变量，可被配置文件覆盖的等价项）。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是 exporter 运行配置。
type Config struct {
	JenkinsURL     string        // JENKINS_URL
	JenkinsUser    string        // JENKINS_USER
	TokenFile      string        // JENKINS_TOKEN_FILE
	ScrapeInterval time.Duration // SCRAPE_INTERVAL
	JobWhitelist   []string      // JOB_WHITELIST（逗号分隔）
	StatePath      string        // STATE_PATH
	ListenAddr     string        // LISTEN_ADDR
	HTTPTimeout    time.Duration // HTTP_TIMEOUT
	Offline        bool          // OFFLINE：用 testdata fixture 离线跑
	FixtureDir     string        // FIXTURE_DIR：离线 fixture 目录
}

// Load 从环境变量加载配置并校验。
func Load() (Config, error) {
	cfg := Config{
		JenkinsURL:     getenv("JENKINS_URL", ""),
		JenkinsUser:    getenv("JENKINS_USER", ""),
		TokenFile:      getenv("JENKINS_TOKEN_FILE", "/vault/secrets/jenkins-token"),
		ScrapeInterval: getDuration("SCRAPE_INTERVAL", 60*time.Second),
		StatePath:      getenv("STATE_PATH", "/data/state.json"),
		ListenAddr:     getenv("LISTEN_ADDR", ":9300"),
		HTTPTimeout:    getDuration("HTTP_TIMEOUT", 10*time.Second),
		Offline:        getBool("OFFLINE", false),
		FixtureDir:     getenv("FIXTURE_DIR", "testdata"),
	}
	cfg.JobWhitelist = parseWhitelist(getenv("JOB_WHITELIST", ""))

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.Offline {
		// 离线模式不要求 Jenkins 凭据，便于无 token 时本地/镜像冒烟。
		return nil
	}
	if c.JenkinsURL == "" {
		return fmt.Errorf("JENKINS_URL 必填")
	}
	if c.JenkinsUser == "" {
		return fmt.Errorf("JENKINS_USER 必填")
	}
	if c.ScrapeInterval <= 0 {
		return fmt.Errorf("SCRAPE_INTERVAL 必须为正")
	}
	if c.HTTPTimeout <= 0 {
		return fmt.Errorf("HTTP_TIMEOUT 必须为正")
	}
	return nil
}

// WhitelistMatch 判断 job 全路径是否在白名单内（精确匹配）。
func (c Config) WhitelistMatch(jobPath string) bool {
	for _, w := range c.JobWhitelist {
		if w == jobPath {
			return true
		}
	}
	return false
}

func parseWhitelist(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func getDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func getBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
