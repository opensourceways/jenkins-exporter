// Package server 暴露 /metrics 与 /healthz HTTP 端点。
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server 包装 /metrics 与 /healthz。
type Server struct {
	addr string
	reg  prometheus.Gatherer
	log  *slog.Logger
	srv  *http.Server
}

// New 创建 server。reg 为 Prometheus registry（Gatherer）。
func New(addr string, reg prometheus.Gatherer, log *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s := &Server{
		addr: addr,
		reg:  reg,
		log:  log,
		srv: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
	return s
}

// ListenAndServe 阻塞运行直到 ctx 取消或监听失败。
func (s *Server) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()
	s.log.Info("http server 启动", "addr", s.addr)
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Handler 返回底层 mux（供测试直接调用）。
func (s *Server) Handler() http.Handler { return s.srv.Handler }
