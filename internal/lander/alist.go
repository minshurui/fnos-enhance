package lander

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// AlistBackend 通过 Alist HTTP API 落地。
//
// 用于光鸭：光鸭的 CloudDrive2 CloudFS 挂载只读，mkdir/rename 均失败，
// 而 Alist 的 GuangYaPan 驱动实测支持 mkdir/rename/move。
//
// 只调 API、不挂 FUSE、不用缓存——因为改名不需要读文件内容。
// 用完即停，常驻开销约等于一个几十 MB 的 Go 进程。
type AlistBackend struct {
	// BaseURL Alist 服务地址，如 http://127.0.0.1:5245
	BaseURL string
	// Token Alist API token；为空时用 Username/Password 登录换取
	Token    string
	Username string
	Password string

	HTTP *http.Client

	mu       sync.Mutex
	token    string
	listOnce map[string][]alistEntry // 目录列表缓存，避免重复请求
}

type alistEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

type alistResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// NewAlistBackend 构造 Alist 后端
func NewAlistBackend(baseURL, token, user, pass string) *AlistBackend {
	return &AlistBackend{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Token:    token,
		Username: user,
		Password: pass,
		HTTP:     &http.Client{Timeout: 90 * time.Second},
		listOnce: make(map[string][]alistEntry),
	}
}

func (a *AlistBackend) Name() string { return "Alist" }

func (a *AlistBackend) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 90 * time.Second}
}

// auth 返回可用 token；必要时登录
func (a *AlistBackend) auth(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Token != "" {
		return a.Token, nil
	}
	if a.token != "" {
		return a.token, nil
	}
	if a.Username == "" || a.Password == "" {
		return "", fmt.Errorf("Alist 未提供 token，也未提供用户名/密码（设置 ALIST_URL/ALIST_TOKEN 或 ALIST_USER/ALIST_PASS）")
	}
	body, _ := json.Marshal(map[string]string{"username": a.Username, "password": a.Password})
	var out struct {
		Token string `json:"token"`
	}
	if err := a.callRaw(ctx, "/api/auth/login", body, "", &out); err != nil {
		return "", fmt.Errorf("Alist 登录失败: %w", err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("Alist 登录未返回 token")
	}
	a.token = out.Token
	return a.token, nil
}

// callRaw 执行一次 API 调用；tok 为空表示不带鉴权
func (a *AlistBackend) callRaw(ctx context.Context, apiPath string, body []byte, tok string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+apiPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", tok)
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var r alistResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("解析响应失败 (HTTP %d): %w", resp.StatusCode, err)
	}
	// Alist 用 body 里的 code 表达业务状态，HTTP 状态码常年 200
	if r.Code != 200 {
		return fmt.Errorf("Alist 返回 code=%d: %s", r.Code, r.Message)
	}
	if out != nil && len(r.Data) > 0 {
		if err := json.Unmarshal(r.Data, out); err != nil {
			return fmt.Errorf("解析 data 失败: %w", err)
		}
	}
	return nil
}

func (a *AlistBackend) call(ctx context.Context, apiPath string, payload map[string]any, out any) error {
	tok, err := a.auth(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return a.callRaw(ctx, apiPath, body, tok, out)
}

// list 列目录；refresh=true 强制穿透缓存（改名后必须刷新才能看到真实状态）
func (a *AlistBackend) list(ctx context.Context, dir string, refresh bool) ([]alistEntry, error) {
	if !refresh {
		a.mu.Lock()
		if c, ok := a.listOnce[dir]; ok {
			a.mu.Unlock()
			return c, nil
		}
		a.mu.Unlock()
	}
	var out struct {
		Content []alistEntry `json:"content"`
	}
	err := a.call(ctx, "/api/fs/list", map[string]any{
		"path": dir, "page": 1, "per_page": 0, "refresh": refresh,
	}, &out)
	if err != nil {
		return nil, fmt.Errorf("列目录失败 %s: %w", dir, err)
	}
	a.mu.Lock()
	a.listOnce[dir] = out.Content
	a.mu.Unlock()
	return out.Content, nil
}

// Walk 递归遍历（BFS，避免深递归；每层一次 API 调用）
func (a *AlistBackend) Walk(ctx context.Context, root string, fn func(rel string) error) error {
	root = "/" + strings.Trim(root, "/")
	queue := []string{root}
	for len(queue) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		dir := queue[0]
		queue = queue[1:]

		entries, err := a.list(ctx, dir, false)
		if err != nil {
			return err
		}
		for _, e := range entries {
			full := path.Join(dir, e.Name)
			if e.IsDir {
				queue = append(queue, full)
				continue
			}
			if strings.HasPrefix(e.Name, ".") {
				continue
			}
			rel := strings.TrimPrefix(strings.TrimPrefix(full, root), "/")
			if err := fn(rel); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *AlistBackend) MkdirAll(ctx context.Context, dir string) error {
	// Alist 的 mkdir 自身支持多级创建
	return a.call(ctx, "/api/fs/mkdir", map[string]any{"path": dir}, nil)
}

func (a *AlistBackend) Exists(ctx context.Context, p string) (bool, error) {
	parent, name := path.Split(strings.TrimRight(p, "/"))
	entries, err := a.list(ctx, strings.TrimRight(parent, "/"), true)
	if err != nil {
		// 父目录不存在时 Alist 报错；对"是否存在"而言等价于不存在
		return false, nil
	}
	for _, e := range entries {
		if e.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// Rename 把 src 改名/移动到 dst。
//
// Alist 没有「同时改名+移动」的单一接口，必须拆两步：
//   - 同目录     → 直接 /api/fs/rename
//   - 跨目录     → 先在源目录内改成目标文件名，再 /api/fs/move 移过去
//
// 之所以「先改名后移动」而不是反过来：移动时若目标目录已存在同名旧文件
// （比如上一轮跑到一半中断留下的），Alist 的行为不确定，可能覆盖。
// 先改名成规范名（规范名批内唯一，已由碰撞检测保证），再移动更安全。
func (a *AlistBackend) Rename(ctx context.Context, src, dst string) error {
	srcDir, srcName := path.Split(src)
	dstDir, dstName := path.Split(dst)
	srcDir = strings.TrimRight(srcDir, "/")
	dstDir = strings.TrimRight(dstDir, "/")

	if srcDir == dstDir {
		if srcName == dstName {
			return nil // 无需操作
		}
		return a.call(ctx, "/api/fs/rename", map[string]any{
			"path": src, "name": dstName,
		}, nil)
	}

	// 跨目录：第一步在源目录内改成最终文件名
	staged := src
	if srcName != dstName {
		if err := a.call(ctx, "/api/fs/rename", map[string]any{
			"path": src, "name": dstName,
		}, nil); err != nil {
			return fmt.Errorf("跨目录落地第一步（源目录内改名）失败: %w", err)
		}
		staged = path.Join(srcDir, dstName)
	}

	// 第二步移动到目标目录
	if err := a.call(ctx, "/api/fs/move", map[string]any{
		"src_dir": srcDir, "dst_dir": dstDir, "names": []string{dstName},
	}, nil); err != nil {
		// 移动失败要说清文件此刻在哪，否则人工无从下手
		return fmt.Errorf("跨目录落地第二步（移动）失败，文件现位于 %s: %w", staged, err)
	}

	// 移动后源目录与目标目录的缓存都失效了
	a.mu.Lock()
	delete(a.listOnce, srcDir)
	delete(a.listOnce, dstDir)
	a.mu.Unlock()
	return nil
}
