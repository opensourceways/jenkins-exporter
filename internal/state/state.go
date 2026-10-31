// Package state 管理 state.json：水位线加载、原子写、损坏自愈与基线。
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// JobState 是单个 job 的增量水位线与计数。
type JobState struct {
	LastSeenBuild        int   `json:"last_seen_build"`
	ConsecutiveFailures  int   `json:"consecutive_failures"`
	LastSuccessTimestamp int64 `json:"last_success_timestamp"`
}

// State 是 state.json 的顶层结构。
type State struct {
	Version int                  `json:"version"`
	Jobs    map[string]*JobState `json:"jobs"`
}

// CurrentVersion 是当前 state 文件版本。
const CurrentVersion = 1

// New 返回空 State。
func New() *State {
	return &State{Version: CurrentVersion, Jobs: map[string]*JobState{}}
}

// Load 从 path 加载 state。文件缺失或 JSON 损坏时丢弃并返回空 State（基线）。
// 返回的 existed 表示文件原本是否存在（用于日志）。
func Load(path string) (*State, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return New(), false, nil
		}
		return nil, false, fmt.Errorf("读取 state 文件失败: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		// 损坏：丢弃重建基线。
		return New(), true, nil
	}
	if s.Jobs == nil {
		s.Jobs = map[string]*JobState{}
	}
	if s.Version == 0 {
		s.Version = CurrentVersion
	}
	return &s, true, nil
}

// Save 原子写：先写 tmp 再 rename 覆盖，避免半写态。
func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建 state 目录失败: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 state 失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写 state tmp 文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename state 文件失败: %w", err)
	}
	return nil
}

// Get 返回 job 的状态，不存在返回 nil。
func (s *State) Get(job string) *JobState {
	return s.Jobs[job]
}

// Ensure 返回 job 的状态，不存在则创建并写入 map。
func (s *State) Ensure(job string) *JobState {
	st, ok := s.Jobs[job]
	if !ok {
		st = &JobState{}
		s.Jobs[job] = st
	}
	return st
}
