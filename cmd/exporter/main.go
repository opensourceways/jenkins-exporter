// Package main 是 jenkins-exporter 入口：加载配置 → 起 collector 后台循环 → 起 /metrics server。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/opensourceways/jenkins-exporter/internal/collector"
	"github.com/opensourceways/jenkins-exporter/internal/config"
	"github.com/opensourceways/jenkins-exporter/internal/jenkins"
	"github.com/opensourceways/jenkins-exporter/internal/offline"
	"github.com/opensourceways/jenkins-exporter/internal/server"
	"github.com/opensourceways/jenkins-exporter/internal/state"

	obslog "github.com/opensourceways/obs-sdk/go/log"
	obsmetrics "github.com/opensourceways/obs-sdk/go/metrics"
)

func main() {
	// 日志与指标都经 obs-sdk 装配：service 显式指定，env/instance/community 由 OBS_* 注入。
	obslog.Init(obslog.Config{Service: "jenkins-exporter"})
	log := obslog.Default()

	cfg, err := config.Load()
	if err != nil {
		log.Error("加载配置失败", "err", err.Error())
		os.Exit(1)
	}

	om := obsmetrics.New(obsmetrics.Config{Service: "jenkins-exporter"})
	metrics := collector.NewMetrics(om)

	st, existed, err := state.Load(cfg.StatePath)
	if err != nil {
		log.Error("加载 state 失败", "err", err.Error())
		os.Exit(1)
	}
	if !existed {
		log.Info("state 文件不存在，首次启动将建基线", "path", cfg.StatePath)
	}

	var src jenkins.DataSource
	if cfg.Offline {
		src = offline.New(cfg.FixtureDir)
		log.Info("离线模式：使用 testdata fixture", "dir", cfg.FixtureDir)
	} else {
		src = jenkins.NewClient(cfg.JenkinsURL, cfg.JenkinsUser, cfg.TokenFile, cfg.HTTPTimeout, log)
		log.Info("在线模式", "jenkins_url", cfg.JenkinsURL, "scrape_interval", cfg.ScrapeInterval.String())
	}

	col := collector.NewCollector(src, cfg.JobWhitelist, st, cfg.StatePath, metrics, cfg.ScrapeInterval, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go col.Run(ctx)

	srv := server.New(cfg.ListenAddr, om.Handler(), log)
	if err := srv.ListenAndServe(ctx); err != nil {
		log.Error("http server 退出", "err", err.Error())
		os.Exit(1)
	}
	log.Info(" exporter 已停止")
}
