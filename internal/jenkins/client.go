// Package jenkins 实现 HTTP 客户端：HTTP Basic 认证、单请求超时、串行、token 文件读取（支持 mtime 轮转）。
package jenkins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// Client 是对 Jenkins REST API 的只读 HTTP 客户端。
// 单实例串行使用；同一时刻不应有并发在途请求（由上层刷新循环保证）。
type Client struct {
	baseURL   string
	username  string
	tokenFile string
	timeout   time.Duration

	mu         sync.Mutex
	token      string
	tokenMtime time.Time
	log        *slog.Logger
}

// NewClient 创建客户端。baseURL 不带末尾斜杠。
func NewClient(baseURL, username, tokenFile string, timeout time.Duration, log *slog.Logger) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		username:  username,
		tokenFile: tokenFile,
		timeout:   timeout,
		log:       log,
	}
}

// Token 返回当前 token（按 mtime 轮转重读）。日志只记长度与 sha256 前 8 位。
func (c *Client) Token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	info, err := os.Stat(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("读取 token 文件元信息失败: %w", err)
	}
	if c.token == "" || !info.ModTime().Equal(c.tokenMtime) {
		data, err := os.ReadFile(c.tokenFile)
		if err != nil {
			return "", fmt.Errorf("读取 token 文件失败: %w", err)
		}
		t := strings.TrimSpace(string(data))
		sum := sha256.Sum256([]byte(t))
		c.log.Info("token 轮转/加载", "len", len(t), "sha256_prefix", hex.EncodeToString(sum[:])[:8])
		c.token = t
		c.tokenMtime = info.ModTime()
	}
	return c.token, nil
}

// jobURL 拼接 job 全路径的 API URL。path 为空表示顶层，直接用 /api/json；
// 否则按 /job/<seg> 逐段拼接。
func (c *Client) jobURL(jobPath, tree string) string {
	u := c.baseURL
	if jobPath != "" {
		for _, s := range strings.Split(jobPath, "/") {
			u += "/job/" + url.PathEscape(s)
		}
	}
	u += "/api/json"
	if tree != "" {
		u += "?tree=" + tree
	}
	return u
}

func (c *Client) apiURL(p, tree string) string {
	u := c.baseURL + path.Join("/", p)
	if !strings.HasSuffix(u, "/api/json") {
		u = strings.TrimRight(u, "/") + "/api/json"
	}
	if tree != "" {
		u += "?tree=" + tree
	}
	return u
}

// do 执行一次 GET，附 Basic 认证，应用超时。
func (c *Client) do(ctx context.Context, fullURL string) ([]byte, int, error) {
	tok, err := c.Token()
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.SetBasicAuth(c.username, tok)

	hc := &http.Client{Timeout: c.timeout}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func (c *Client) getJSON(ctx context.Context, fullURL string, target any) (int, error) {
	body, code, err := c.do(ctx, fullURL)
	if err != nil {
		return code, err
	}
	if code == http.StatusUnauthorized {
		return code, ErrUnauthorized
	}
	if code != http.StatusOK {
		return code, fmt.Errorf("jenkins 返回非预期状态码 %d", code)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return code, fmt.Errorf("解析 jenkins 响应失败: %w", err)
	}
	return code, nil
}

// ErrUnauthorized 表示 Jenkins 返回 401。
var ErrUnauthorized = fmt.Errorf("401 Unauthorized: 检查 token 与用户名")

// Jobs 拉取指定路径（空串为顶层）下的 job 列表。
func (c *Client) Jobs(ctx context.Context, jobPath string) ([]Job, error) {
	full := c.jobURL(jobPath, "jobs[name,_class,url]")
	var resp JobsResponse
	if _, err := c.getJSON(ctx, full, &resp); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

// Builds 拉取指定 job 全路径最近 100 条构建。
func (c *Client) Builds(ctx context.Context, jobPath string) ([]Build, error) {
	full := c.jobURL(jobPath, "builds[number,result,duration,timestamp,builtOn]{0,100}")
	var resp BuildsResponse
	if _, err := c.getJSON(ctx, full, &resp); err != nil {
		return nil, err
	}
	return resp.Builds, nil
}

// Computers 拉取全部节点状态。
func (c *Client) Computers(ctx context.Context) ([]Computer, error) {
	full := c.apiURL("computer", "computer[displayName,offline,temporarilyOffline,numExecutors,busyExecutors]")
	var resp ComputersResponse
	if _, err := c.getJSON(ctx, full, &resp); err != nil {
		return nil, err
	}
	return resp.Computer, nil
}

// Queue 拉取构建队列。
func (c *Client) Queue(ctx context.Context) ([]QueueItem, error) {
	full := c.apiURL("queue", "items[id,why,inQueueSince,task[name]]")
	var resp QueueResponse
	if _, err := c.getJSON(ctx, full, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}
