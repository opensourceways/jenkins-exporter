// Package offline 提供 testdata fixture 离线数据源，用于无 token 时本地/镜像冒烟。
package offline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/opensourceways/jenkins-exporter/internal/jenkins"
)

// Source 从 fixture 目录读取脱敏 JSON，实现 jenkins.DataSource。
type Source struct {
	dir string
}

// New 创建离线数据源。dir 下应含 jobs.json/builds.json/computer.json/queue.json。
func New(dir string) *Source {
	return &Source{dir: dir}
}

func (s *Source) read(name string, target any) error {
	p := filepath.Join(s.dir, name)
	data, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("读取离线 fixture %s 失败: %w", name, err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("解析离线 fixture %s 失败: %w", name, err)
	}
	return nil
}

// Jobs 返回 jobs.json 中的叶子 job（fixture 不含 folder，直接返回全部）。
func (s *Source) Jobs(_ context.Context, path string) ([]jenkins.Job, error) {
	if path != "" {
		// 离线 fixture 不模拟 folder 下钻，子路径返回空。
		return nil, nil
	}
	var resp jenkins.JobsResponse
	if err := s.read("jobs.json", &resp); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

// Builds 返回 builds.json（对所有 job 返回同一份样例）。
func (s *Source) Builds(_ context.Context, _ string) ([]jenkins.Build, error) {
	var resp jenkins.BuildsResponse
	if err := s.read("builds.json", &resp); err != nil {
		return nil, err
	}
	return resp.Builds, nil
}

// Computers 返回 computer.json。
func (s *Source) Computers(_ context.Context) ([]jenkins.Computer, error) {
	var resp jenkins.ComputersResponse
	if err := s.read("computer.json", &resp); err != nil {
		return nil, err
	}
	return resp.Computer, nil
}

// Queue 返回 queue.json。
func (s *Source) Queue(_ context.Context) ([]jenkins.QueueItem, error) {
	var resp jenkins.QueueResponse
	if err := s.read("queue.json", &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}
