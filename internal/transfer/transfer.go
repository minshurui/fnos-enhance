package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	"fnos-enhance/internal/linker"
)

// ------------------------------------------------------------
// 配置：用 struct 而非多个同类型 string 参数
//
// 审计 P0-6 根因：旧接口 NewTransferor(quarkCookie, baiduCookie,
// guangyaClientID, guangyaClientSecret) 四个同类型 string，
// 编译器无法拦截错位；实际把 ClientID 传进去但 Transfer() 只读
// AccessToken，导致光鸭 100% 失败且无人发现。
// ------------------------------------------------------------

// QuarkConfig 夸克网盘配置
type QuarkConfig struct {
	Cookie string // 必填：含 __puus 等的完整 cookie
	// ToDirFID 转存目标目录 fid，空则转存到根目录 "0"
	ToDirFID string
}

// BaiduConfig 百度网盘配置
type BaiduConfig struct {
	Cookie string // 必填：含 BDUSS 的完整 cookie
	// ToDir 转存目标目录路径，空则 "/"
	ToDir string
}

// GuangYaConfig 光鸭云盘配置
type GuangYaConfig struct {
	// AccessToken 优先使用；过期时用 RefreshToken + ClientID 自动刷新
	AccessToken  string
	RefreshToken string
	ClientID     string
	// ToDirID 转存目标目录 ID，空则根目录
	ToDirID string
}

// Ready 是否具备可用凭据
func (c QuarkConfig) Ready() bool { return c.Cookie != "" }
func (c BaiduConfig) Ready() bool { return c.Cookie != "" }
func (c GuangYaConfig) Ready() bool {
	return c.AccessToken != "" || (c.RefreshToken != "" && c.ClientID != "")
}

// Config 转存器总配置
type Config struct {
	Quark   QuarkConfig
	Baidu   BaiduConfig
	GuangYa GuangYaConfig

	// Timeout 单次 HTTP 请求超时，默认 30s
	Timeout time.Duration
	// MaxRetries 限流/5xx 时的重试次数，默认 3
	MaxRetries int
	// PageSize 列表分页大小，默认 100（夸克上限 50 会自动收敛）
	PageSize int
	// MaxDepth 递归列举子目录的最大深度，默认 3（0=不递归）
	MaxDepth int
}

// Validate 构造期校验：至少一家网盘可用，且填充默认值
func (c *Config) Validate() error {
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if c.PageSize <= 0 {
		c.PageSize = 100
	}
	if c.MaxDepth < 0 {
		c.MaxDepth = 0
	}
	if c.MaxDepth == 0 {
		c.MaxDepth = 3
	}
	if !c.Quark.Ready() && !c.Baidu.Ready() && !c.GuangYa.Ready() {
		return fmt.Errorf("三家网盘凭据均为空，至少需配置一家（QUARK_COOKIE / BAIDU_COOKIE / GUANGYA_ACCESS_TOKEN）")
	}
	return nil
}

// ------------------------------------------------------------
// 结果与接口
// ------------------------------------------------------------

// Entry 分享内的一个条目
type Entry struct {
	ID     string // 网盘侧 ID（fid / fs_id / fileId）
	Name   string
	IsDir  bool
	Size   int64
	Parent string // 父目录 ID，根目录条目为空
	Depth  int    // 相对分享根的深度，0 = 顶层

	// shareFidToken 夸克 save 接口要求的 share_fid_token，仅内部使用
	shareFidToken string
	// path 百度递归需要的绝对路径，仅内部使用
	path string
}

// TransferResult 转存结果
type TransferResult struct {
	Provider string   // 夸克/百度/光鸭
	Link     string   // 原始链接
	Names    []string // 顶层条目名（兼容旧字段）
	Entries  []Entry  // 完整条目（含递归列举结果）
	// Transferred 实际提交转存的顶层 ID 数
	Transferred int
	// DryRun 为 true 时只列举不转存
	DryRun bool
}

// FileCount 递归条目里的文件数（不含目录）
func (r *TransferResult) FileCount() int {
	n := 0
	for _, e := range r.Entries {
		if !e.IsDir {
			n++
		}
	}
	return n
}

// Transferor 转存接口
type Transferor interface {
	// Transfer 转存分享到自己网盘；dryRun=true 时只列举不落盘
	Transfer(ctx context.Context, link linker.ShareLink, dryRun bool) (*TransferResult, error)
}

// New 根据配置创建转存器（构造期校验，避免参数错位）
func New(cfg Config) (Transferor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	m := &multiTransferor{cfg: cfg}
	if cfg.Quark.Ready() {
		m.quark = &QuarkTransferor{cfg: cfg.Quark, common: cfg, HTTP: newHTTPClient(cfg.Timeout, false)}
	}
	if cfg.Baidu.Ready() {
		// 百度必须带 CookieJar：verify 后的 BDCLND 要跨请求携带（审计 P1-1）
		m.baidu = &BaiduTransferor{cfg: cfg.Baidu, common: cfg, HTTP: newHTTPClient(cfg.Timeout, true)}
	}
	if cfg.GuangYa.Ready() {
		m.guangya = &GuangYaTransferor{cfg: cfg.GuangYa, common: cfg, HTTP: newHTTPClient(cfg.Timeout, false)}
	}
	return m, nil
}

type multiTransferor struct {
	cfg     Config
	quark   *QuarkTransferor
	baidu   *BaiduTransferor
	guangya *GuangYaTransferor
}

func (m *multiTransferor) Transfer(ctx context.Context, link linker.ShareLink, dryRun bool) (*TransferResult, error) {
	switch link.Type {
	case linker.LinkQuark:
		if m.quark == nil {
			return nil, fmt.Errorf("夸克凭据未配置（设置 QUARK_COOKIE）")
		}
		return m.quark.Transfer(ctx, link, dryRun)
	case linker.LinkBaidu:
		if m.baidu == nil {
			return nil, fmt.Errorf("百度凭据未配置（设置 BAIDU_COOKIE）")
		}
		return m.baidu.Transfer(ctx, link, dryRun)
	case linker.LinkGuangYa:
		if m.guangya == nil {
			return nil, fmt.Errorf("光鸭凭据未配置（设置 GUANGYA_ACCESS_TOKEN 或 GUANGYA_REFRESH_TOKEN+GUANGYA_CLIENT_ID）")
		}
		return m.guangya.Transfer(ctx, link, dryRun)
	default:
		return nil, fmt.Errorf("不支持的链接类型: %s", link.Link)
	}
}

// ------------------------------------------------------------
// 共用 HTTP：显式 method + 重试 + context
// ------------------------------------------------------------

func newHTTPClient(timeout time.Duration, withJar bool) *http.Client {
	c := &http.Client{Timeout: timeout}
	if withJar {
		jar, err := cookiejar.New(nil)
		if err == nil {
			c.Jar = jar
		}
	}
	return c
}

// httpError 带状态码的错误，便于判断是否可重试
type httpError struct {
	Code int
	Body string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Code, truncate(e.Body, 150))
}

func (e *httpError) retryable() bool {
	return e.Code == 429 || e.Code == 408 || (e.Code >= 500 && e.Code < 600)
}

// doJSON 发起请求并解析 JSON，带指数退避重试（审计 P1-7）
// method 必须显式传（审计 P2：旧代码用 data==nil 隐式决定 GET/POST）
func doJSON(ctx context.Context, client *http.Client, maxRetries int,
	method, rawURL string, jsonBody map[string]interface{},
	setHeaders func(*http.Request), out interface{}) error {

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// 指数退避 + 抖动，避免同时重试打爆网盘
			back := time.Duration(1<<uint(attempt-1)) * time.Second
			back += time.Duration(rand.Intn(300)) * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(back):
			}
		}

		var body io.Reader
		if jsonBody != nil {
			b, err := json.Marshal(jsonBody)
			if err != nil {
				return err
			}
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
		if err != nil {
			return err
		}
		if jsonBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if setHeaders != nil {
			setHeaders(req)
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			continue // 网络抖动，重试
		}
		buf, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			he := &httpError{Code: resp.StatusCode, Body: string(buf)}
			if he.retryable() && attempt < maxRetries {
				lastErr = he
				continue
			}
			return he
		}

		if out == nil {
			return nil
		}
		if err := json.Unmarshal(buf, out); err != nil {
			return fmt.Errorf("响应解析失败(%s): %s", err, truncate(string(buf), 150))
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("重试 %d 次后仍失败", maxRetries)
	}
	return lastErr
}

// doForm 发起表单请求（百度用），带重试；返回原始字节
func doForm(ctx context.Context, client *http.Client, maxRetries int,
	method, rawURL, form string, setHeaders func(*http.Request)) ([]byte, error) {

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			back := time.Duration(1<<uint(attempt-1)) * time.Second
			back += time.Duration(rand.Intn(300)) * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(back):
			}
		}

		var body io.Reader
		if form != "" {
			body = strings.NewReader(form)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
		if err != nil {
			return nil, err
		}
		if form != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if setHeaders != nil {
			setHeaders(req)
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		buf, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode != http.StatusOK {
			he := &httpError{Code: resp.StatusCode, Body: string(buf)}
			if he.retryable() && attempt < maxRetries {
				lastErr = he
				continue
			}
			return nil, he
		}
		return buf, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("重试 %d 次后仍失败", maxRetries)
	}
	return nil, lastErr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
