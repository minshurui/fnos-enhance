package transfer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"fnos-enhance/internal/linker"
)

// GuangYaTransferor 光鸭云盘转存器
//
// 审计 P0-6 修复：旧实现由 NewTransferor 传入 ClientID/ClientSecret，
// 但 Transfer() 只读 AccessToken，导致永远返回"access_token 未配置"。
// 现在改为：AccessToken 优先；为空且有 RefreshToken+ClientID 则自动刷新；
// 构造期已由 Config.Validate + GuangYaConfig.Ready 拦截空凭据。
type GuangYaTransferor struct {
	cfg    GuangYaConfig
	common Config
	HTTP   *http.Client

	mu          sync.Mutex
	cachedToken string

	// apiBase / authURL 仅供测试注入，空则用真实域名
	apiBase string
	authURL string
}

func (g *GuangYaTransferor) base() string {
	if g.apiBase != "" {
		return g.apiBase
	}
	return guangyaAPIBase
}

func (g *GuangYaTransferor) auth() string {
	if g.authURL != "" {
		return g.authURL
	}
	return guangyaAuthURL
}

const guangyaAPIBase = "https://api.guangyapan.com"
const guangyaAuthURL = "https://account.guangyapan.com/v1/auth/token"

type guangyaFileItem struct {
	FileID   string      `json:"fileId"`
	ID       string      `json:"id"`
	FileName string      `json:"fileName"`
	Name     string      `json:"name"`
	FileType json.Number `json:"fileType"`
	IsDir    bool        `json:"isDir"`
	Folder   bool        `json:"folder"`
	Size     json.Number `json:"size"`
}

func (i guangyaFileItem) id() string {
	if i.FileID != "" {
		return i.FileID
	}
	return i.ID
}

func (i guangyaFileItem) name() string {
	if i.FileName != "" {
		return i.FileName
	}
	return i.Name
}

func (i guangyaFileItem) isDir() bool {
	if i.IsDir || i.Folder {
		return true
	}
	// fileType: 0/1 语义各版本不一，仅当明确为 "0" 且无其他线索时判目录
	return i.FileType.String() == "0"
}

type guangyaListResp struct {
	Code json.Number `json:"code"`
	Msg  string      `json:"msg"`
	Data struct {
		FileList []guangyaFileItem `json:"fileList"`
		List     []guangyaFileItem `json:"list"`
		Total    json.Number       `json:"total"`
	} `json:"data"`
}

func (r guangyaListResp) items() []guangyaFileItem {
	if len(r.Data.FileList) > 0 {
		return r.Data.FileList
	}
	return r.Data.List
}

// accessToken 取可用 token：缓存 → 配置 → 过期则刷新
func (g *GuangYaTransferor) accessToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.cachedToken != "" && !jwtExpired(g.cachedToken) {
		return g.cachedToken, nil
	}
	if g.cfg.AccessToken != "" && !jwtExpired(g.cfg.AccessToken) {
		g.cachedToken = g.cfg.AccessToken
		return g.cachedToken, nil
	}
	// AccessToken 缺失或已过期 → 用 refresh_token 换新
	if g.cfg.RefreshToken == "" || g.cfg.ClientID == "" {
		if g.cfg.AccessToken != "" {
			return "", fmt.Errorf("光鸭 access_token 已过期，且未配置 GUANGYA_REFRESH_TOKEN + GUANGYA_CLIENT_ID 无法自动刷新")
		}
		return "", fmt.Errorf("光鸭凭据未配置：需 GUANGYA_ACCESS_TOKEN，或 GUANGYA_REFRESH_TOKEN + GUANGYA_CLIENT_ID")
	}

	var ar struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	err := doJSON(ctx, g.HTTP, g.common.MaxRetries, http.MethodPost, g.auth(),
		map[string]interface{}{
			"client_id":     g.cfg.ClientID,
			"grant_type":    "refresh_token",
			"refresh_token": g.cfg.RefreshToken,
		},
		func(req *http.Request) { req.Header.Set("User-Agent", browserUA) }, &ar)
	if err != nil {
		return "", fmt.Errorf("光鸭 token 刷新请求失败: %w", err)
	}
	if ar.AccessToken == "" {
		return "", fmt.Errorf("光鸭 token 刷新失败: %s %s（refresh_token 可能已失效，需重新授权）", ar.Error, ar.ErrorDesc)
	}
	g.cachedToken = ar.AccessToken
	return g.cachedToken, nil
}

func (g *GuangYaTransferor) headers(token string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("User-Agent", browserUA)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
}

// Transfer 光鸭分享转存：share_access_token → files(分页+递归) → restore_share
func (g *GuangYaTransferor) Transfer(ctx context.Context, link linker.ShareLink, dryRun bool) (*TransferResult, error) {
	if link.ID == "" {
		return nil, fmt.Errorf("无法解析光鸭链接: %s", link.Link)
	}
	userToken, err := g.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	shareID := link.ID

	// 1. 换取分享访问 token
	var sr struct {
		Code json.Number `json:"code"`
		Msg  string      `json:"msg"`
		Data struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	err = doJSON(ctx, g.HTTP, g.common.MaxRetries, http.MethodPost,
		g.base()+"/userres/v1/get_share_access_token",
		map[string]interface{}{"shareId": shareID, "sharePwd": link.Pwd},
		g.headers(""), &sr)
	if err != nil {
		return nil, fmt.Errorf("光鸭取分享 token 失败: %w", err)
	}
	if sr.Data.AccessToken == "" {
		return nil, fmt.Errorf("光鸭分享不可访问: code=%s msg=%s（可能已失效或需提取码）", sr.Code.String(), sr.Msg)
	}
	sat := sr.Data.AccessToken

	// 2. 列举顶层 + 递归（审计 P0-4：旧代码 parentId="" 单页不递归）
	top, err := g.listDir(ctx, sat, "", 0)
	if err != nil {
		return nil, err
	}
	if len(top) == 0 {
		return nil, fmt.Errorf("分享无文件")
	}

	all := append([]Entry(nil), top...)
	if g.common.MaxDepth > 0 {
		sub, err := g.walk(ctx, sat, top, 1)
		if err != nil {
			fmt.Printf("警告: 光鸭递归列举中断（%v），仅按顶层条目转存\n", err)
		} else {
			all = append(all, sub...)
		}
	}

	names := make([]string, 0, len(top))
	fileIDs := make([]string, 0, len(top))
	for _, e := range top {
		names = append(names, e.Name)
		fileIDs = append(fileIDs, e.ID)
	}

	result := &TransferResult{
		Provider: "光鸭", Link: link.Link,
		Names: names, Entries: all, DryRun: dryRun,
	}
	if dryRun {
		return result, nil
	}

	// 3. 转存
	var rr struct {
		Code json.Number `json:"code"`
		Msg  string      `json:"msg"`
	}
	err = doJSON(ctx, g.HTTP, g.common.MaxRetries, http.MethodPost,
		g.base()+"/userres/v1/restore_share",
		map[string]interface{}{
			"accessToken": sat,
			"fileIds":     fileIDs,
			"parentId":    g.cfg.ToDirID,
		}, g.headers(userToken), &rr)
	if err != nil {
		return nil, fmt.Errorf("光鸭转存请求失败: %w", err)
	}
	if !strings.EqualFold(rr.Msg, "success") && rr.Code.String() != "0" && rr.Code.String() != "200" {
		return nil, fmt.Errorf("光鸭转存被拒: code=%s msg=%s", rr.Code.String(), rr.Msg)
	}

	result.Transferred = len(fileIDs)
	return result, nil
}

// listDir 列举分享内某目录（自动翻页）
func (g *GuangYaTransferor) listDir(ctx context.Context, sat, parentID string, depth int) ([]Entry, error) {
	size := g.common.PageSize
	if size <= 0 || size > 200 {
		size = 100
	}

	var out []Entry
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var lr guangyaListResp
		err := doJSON(ctx, g.HTTP, g.common.MaxRetries, http.MethodPost,
			g.base()+"/userres/v1/get_share_page_files_list",
			map[string]interface{}{
				"accessToken": sat,
				"parentId":    parentID,
				"pageSize":    size,
				"pageNum":     page,
				"orderBy":     0,
				"sortType":    0,
			}, g.headers(""), &lr)
		if err != nil {
			return out, fmt.Errorf("光鸭列举目录 %q 第 %d 页失败: %w", parentID, page, err)
		}
		items := lr.items()
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			id := it.id()
			if id == "" {
				continue
			}
			size64, _ := it.Size.Int64()
			out = append(out, Entry{
				ID: id, Name: it.name(), IsDir: it.isDir(),
				Size: size64, Parent: parentID, Depth: depth,
			})
		}
		if total, err := lr.Data.Total.Int64(); err == nil && total > 0 && int64(len(out)) >= total {
			break
		}
		if len(items) < size {
			break
		}
		if page > 200 {
			return out, fmt.Errorf("光鸭分页超过 200 页，疑似接口异常")
		}
	}
	return out, nil
}

// walk 递归列举子目录
func (g *GuangYaTransferor) walk(ctx context.Context, sat string, parents []Entry, depth int) ([]Entry, error) {
	if depth > g.common.MaxDepth {
		return nil, nil
	}
	var out []Entry
	for _, p := range parents {
		if !p.IsDir {
			continue
		}
		kids, err := g.listDir(ctx, sat, p.ID, depth)
		if err != nil {
			return out, err
		}
		out = append(out, kids...)
		deeper, err := g.walk(ctx, sat, kids, depth+1)
		if err != nil {
			return out, err
		}
		out = append(out, deeper...)
	}
	return out, nil
}

// jwtExpired 解析 JWT payload 的 exp 判断是否过期（提前 60s 视为过期）
func jwtExpired(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return false // 非 JWT 格式，交给服务端判断
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad > 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return false
		}
	}
	var p struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Exp == 0 {
		return false
	}
	return p.Exp < time.Now().Add(60*time.Second).Unix()
}
