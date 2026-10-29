// Package jenkins 封装对 Jenkins REST API 的只读访问与数据结构。
package jenkins

import (
	"context"
	"strings"
)

// Job 表示 Jenkins 顶层或 folder 内的 job 条目。
type Job struct {
	Name  string `json:"name"`
	Class string `json:"_class"`
	URL   string `json:"url"`
}

// JobsResponse 对应 GET /api/json?tree=jobs[name,_class,url]。
type JobsResponse struct {
	Jobs []Job `json:"jobs"`
}

// Build 表示一条构建记录。Result 为 nil 表示构建仍在运行（未完成）。
type Build struct {
	Number    int     `json:"number"`
	Result    *string `json:"result"`
	Duration  int64   `json:"duration"`
	Timestamp int64   `json:"timestamp"`
	BuiltOn   string  `json:"builtOn"`
}

// BuildsResponse 对应 GET /job/<path>/api/json?tree=builds[...]{0,100}。
type BuildsResponse struct {
	Builds []Build `json:"builds"`
}

// Computer 表示一个 Jenkins 节点（agent/controller）。
type Computer struct {
	DisplayName        string `json:"displayName"`
	Offline            bool   `json:"offline"`
	TemporarilyOffline bool   `json:"temporarilyOffline"`
	NumExecutors       int    `json:"numExecutors"`
	BusyExecutors      int    `json:"busyExecutors"`
}

// ComputersResponse 对应 GET /computer/api/json?tree=computer[...]。
type ComputersResponse struct {
	Computer []Computer `json:"computer"`
}

// QueueItem 表示 Jenkins 构建队列中的一条排队项。
type QueueItem struct {
	ID           int    `json:"id"`
	Why          string `json:"why"`
	InQueueSince int64  `json:"inQueueSince"`
	Task         struct {
		Name string `json:"name"`
	} `json:"task"`
}

// QueueResponse 对应 GET /queue/api/json?tree=items[...]。
type QueueResponse struct {
	Items []QueueItem `json:"items"`
}

// DataSource 抽象 Jenkins 四类只读数据拉取，便于用 httptest 桩与离线 fixture 复用。
type DataSource interface {
	Jobs(ctx context.Context, path string) ([]Job, error)
	Builds(ctx context.Context, jobPath string) ([]Build, error)
	Computers(ctx context.Context) ([]Computer, error)
	Queue(ctx context.Context) ([]QueueItem, error)
}

// 已知构建结果枚举。
const (
	ResultSuccess  = "SUCCESS"
	ResultFailure  = "FAILURE"
	ResultAborted  = "ABORTED"
	ResultUnstable = "UNSTABLE"
)

// IsKnownResult 判断 result 是否属于 counter 计数的四种已知值。
func IsKnownResult(r string) bool {
	switch r {
	case ResultSuccess, ResultFailure, ResultAborted, ResultUnstable:
		return true
	}
	return false
}

// IsFolderClass 判断 _class 是否为 folder 类型（需递归下钻）。
func IsFolderClass(class string) bool {
	return strings.Contains(class, "Folder")
}
