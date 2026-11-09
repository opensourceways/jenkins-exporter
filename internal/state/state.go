// Package state 管理 state.json：水位线加载、原子写、损坏自愈与基线。
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

	// RedSeen 是上一轮处于 red 的 job 全路径，用于「连续两轮 red」判定。
	RedSeen []string `json:"red_seen,omitempty"`
	// Alerted 是已告警过、且仍处于 red 的 job，避免同一故障每轮重复告警。
	Alerted []string `json:"alerted,omitempty"`
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

// RedMaps 把持久化的 red 集合转为 map，便于 collector 做差集。
func (s *State) RedMaps() (seen, alerted map[string]bool) {
	seen = make(map[string]bool, len(s.RedSeen))
	for _, j := range s.RedSeen {
		seen[j] = true
	}
	alerted = make(map[string]bool, len(s.Alerted))
	for _, j := range s.Alerted {
		alerted[j] = true
	}
	return seen, alerted
}

// SetRedState 写回 red 集合。排序后写入，保证同内容产生相同文件（便于 diff 与测试）。
func (s *State) SetRedState(seen, alerted map[string]bool) {
	s.RedSeen = sortedKeys(seen)
	s.Alerted = sortedKeys(alerted)
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
