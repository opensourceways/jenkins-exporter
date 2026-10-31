// Package server 暴露 /metrics 与 /healthz HTTP 端点。
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Server 包装 /metrics 与 /healthz。
type Server struct {
	addr string
	log  *slog.Logger
	srv  *http.Server
}

// New 创建 server。metrics 为 /metrics 的 handler（obs-sdk 的 Metrics.Handler()）。
func New(addr string, metrics http.Handler, log *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s := &Server{
		addr: addr,
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
