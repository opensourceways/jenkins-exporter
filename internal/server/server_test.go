package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestHealthz(t *testing.T) {
	reg := prometheus.NewRegistry()
	s := New(":0", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), testLog())
	rw := httptest.NewRecorder()
	s.Handler().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rw.Code != http.StatusOK {
		t.Errorf("healthz code = %d, want 200", rw.Code)
	}
	body, _ := io.ReadAll(rw.Body)
	if string(body) != "ok" {
		t.Errorf("healthz body = %q", body)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "jenkins_test_metric", Help: "测试"})
	reg.MustRegister(g)
	g.Set(42)
	s := New(":0", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), testLog())
	rw := httptest.NewRecorder()
	s.Handler().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rw.Code != http.StatusOK {
		t.Errorf("metrics code = %d", rw.Code)
	}
	body, _ := io.ReadAll(rw.Body)
	if !strings.Contains(string(body), "jenkins_test_metric") {
		t.Errorf("metrics 缺少指标输出: %s", body)
	}
	if !strings.Contains(string(body), "# TYPE jenkins_test_metric gauge") {
		t.Errorf("metrics 缺少 TYPE 行")
	}
}

func TestListenAndServeShutdown(t *testing.T) {
	reg := prometheus.NewRegistry()
	s := New("127.0.0.1:0", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), testLog())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ListenAndServe 返回 err: %v", err)
		}
	}
}
