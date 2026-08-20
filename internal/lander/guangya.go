package lander

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// GuangYaBackend 直连光鸭云盘官方 API 落地，不需要 Alist、不需要挂载。
//
// 协议是从 AlistGo/alist 的 drivers/guangyapan 驱动移植的（开源，MIT）。
// 之所以能剥离：光鸭 API 就是普通 JSON POST + Bearer token，
// 无签名、无加密、无设备指纹校验，Alist 在中间没做任何特殊的事。
//
// 端点（基址 https://api.guangyapan.com）：
//
//	列目录   POST /userres/v1/file/get_file_list        {parentId,page,pageSize,orderBy,sortType,fileTypes}
//	建目录   POST /nd.bizuserres.s/v1/file/create_dir   {parentId,dirName}
//	改名     POST /nd.bizuserres.s/v1/file/rename       {fileId,newName}
//	移动     POST /nd.bizuserres.s/v1/file/move_file    {fileIds,parentId} → taskId
//	任务状态 POST /nd.bizuserres.s/v1/get_task_status    {taskId}
//	刷新令牌 POST https://account.guangyapan.com/v1/auth/token {client_id,grant_type,refresh_token}
//
// 注意：根目录的 parentId 是**空字符串**，不是 "0" 或 "/"。
type GuangYaBackend struct {
	AccessToken  string
	RefreshToken string
	ClientID     string

	// APIBase / AccountBase 可注入用于测试
	APIBase     string
	AccountBase string

	HTTP *http.Client

	// TokenSink 令牌刷新后的回调（持久化用）；可为 nil
	TokenSink func(access, refresh string)

	mu sync.Mutex
	// idCache 路径 → fileId，避免重复列目录。根路径 "" 对应空 ID
	idCache map[string]string
	// dirty 标记某目录的列表缓存已失效
	listCache map[string][]gyItem

	// rl 每端点限流。光鸭对同一端点有频率限制，打太快会返回
	// 「成功但没有 list 字段」的响应 —— 如果当成空目录就会静默丢文件。
	// 间隔与 Alist 驱动一致（实测值，不是猜的）。
	rl *rateLimiter
}

// rateLimiter 按端点路径限流，无外部依赖
type rateLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
	gap  time.Duration
}

func newRateLimiter(gap time.Duration) *rateLimiter {
	return &rateLimiter{last: map[string]time.Time{}, gap: gap}
}

func (r *rateLimiter) wait(ctx context.Context, key string) error {
	r.mu.Lock()
	now := time.Now()
	var d time.Duration
	if next := r.last[key].Add(r.gap); next.After(now) {
		d = next.Sub(now)
	}
	// 预占时隙，保证并发调用也能排队
	r.last[key] = now.Add(d)
	r.mu.Unlock()

	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

type gyItem struct {
	FileID   string `json:"fileId"`
	ParentID string `json:"parentId"`
	FileName string `json:"fileName"`
	FileSize int64  `json:"fileSize"`
	ResType  int    `json:"resType"` // 2 = 目录
}

func (i gyItem) IsDir() bool { return i.ResType == 2 }

// NewGuangYaBackend 构造光鸭原生后端
func NewGuangYaBackend(access, refresh, clientID string) *GuangYaBackend {
	return &GuangYaBackend{
		AccessToken:  access,
		RefreshToken: refresh,
		ClientID:     clientID,
		APIBase:      "https://api.guangyapan.com",
		AccountBase:  "https://account.guangyapan.com",
		HTTP:         &http.Client{Timeout: 60 * time.Second},
		idCache:      map[string]string{"": ""},
		listCache:    map[string][]gyItem{},
		rl:           newRateLimiter(500 * time.Millisecond),
	}
}

func (g *GuangYaBackend) Name() string { return "光鸭(原生API)" }

func (g *GuangYaBackend) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// post 调 API；401/403 时自动刷新令牌重试一次
func (g *GuangYaBackend) post(ctx context.Context, apiPath string, body, out any) error {
	if g.rl != nil {
		if err := g.rl.wait(ctx, apiPath); err != nil {
			return err
		}
	}
	do := func() (int, []byte, error) {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.APIBase+apiPath, bytes.NewReader(b))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		g.mu.Lock()
		tok := g.AccessToken
		g.mu.Unlock()
		if tok == "" {
			return 0, nil, fmt.Errorf("光鸭 access_token 为空")
		}
		req.Header.Set("Authorization", "Bearer "+tok)

		resp, err := g.client().Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		buf := new(bytes.Buffer)
		if _, err := buf.ReadFrom(resp.Body); err != nil {
			return resp.StatusCode, nil, err
		}
		return resp.StatusCode, buf.Bytes(), nil
	}

	code, raw, err := do()
	if err != nil {
		return err
	}
	if code == 401 || code == 403 {
		if err := g.refresh(ctx); err != nil {
			return fmt.Errorf("令牌过期且刷新失败: %w", err)
		}
		code, raw, err = do()
		if err != nil {
			return err
		}
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("光鸭 API %s 失败: HTTP %d %s", apiPath, code, truncStr(string(raw), 200))
	}

	// 业务层用 msg=="success" 表示成功，HTTP 200 不代表业务成功
	var probe struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(probe.Msg), "success") {
		return fmt.Errorf("光鸭 API %s 返回 code=%d msg=%q", apiPath, probe.Code, probe.Msg)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("解析 data 失败: %w", err)
		}
	}
	return nil
}

// refresh 用 refresh_token 换新的 access_token
func (g *GuangYaBackend) refresh(ctx context.Context) error {
	g.mu.Lock()
	rt, cid := g.RefreshToken, g.ClientID
	g.mu.Unlock()
	if rt == "" {
		return fmt.Errorf("refresh_token 为空")
	}
	b, _ := json.Marshal(map[string]any{
		"client_id":     cid,
		"grant_type":    "refresh_token",
		"refresh_token": rt,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.AccountBase+"/v1/auth/token", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if out.AccessToken == "" {
		msg := out.ErrorDesc
		if msg == "" {
			msg = out.Error
		}
		return fmt.Errorf("刷新令牌失败: %s (HTTP %d)", msg, resp.StatusCode)
	}
	g.mu.Lock()
	g.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		g.RefreshToken = out.RefreshToken
	}
	access, refresh := g.AccessToken, g.RefreshToken
	sink := g.TokenSink
	g.mu.Unlock()

	// 令牌轮换后必须持久化，否则下次运行还用旧的
	if sink != nil {
		sink(access, refresh)
	}
	fmt.Fprintln(os.Stderr, "提示: 光鸭令牌已刷新")
	return nil
}

// listDir 列出目录内容（按 fileId）
func (g *GuangYaBackend) listDir(ctx context.Context, parentID string, refresh bool) ([]gyItem, error) {
	if !refresh {
		g.mu.Lock()
		if c, ok := g.listCache[parentID]; ok {
			g.mu.Unlock()
			return c, nil
		}
		g.mu.Unlock()
	}

	const pageSize = 100
	var all []gyItem
	for page := 0; ; page++ {
		var resp struct {
			Data struct {
				Total int       `json:"total"`
				List  *[]gyItem `json:"list"` // 指针：区分「字段缺失」与「空数组」
			} `json:"data"`
		}
		// 被限流时接口会返回「成功但没有 list」，必须重试而不是当成空目录，
		// 否则整个目录的文件会被静默跳过（实测漏过 4 个目录 6 个文件）。
		var lastErr error
		got := false
		for try := 0; try < 3; try++ {
			lastErr = g.post(ctx, "/userres/v1/file/get_file_list", map[string]any{
				"parentId":  parentID,
				"page":      page,
				"pageSize":  pageSize,
				"orderBy":   3,
				"sortType":  1,
				"fileTypes": []int{},
			}, &resp)
			if lastErr != nil {
				return nil, lastErr
			}
			if resp.Data.List != nil {
				got = true
				break
			}
			// list 缺失：第一页且 total>0 说明是异常，重试；后续页说明正常翻完了
			if page > 0 || resp.Data.Total == 0 {
				got = true
				break
			}
			time.Sleep(time.Duration(try+1) * time.Second)
		}
		if !got {
			return nil, fmt.Errorf("列目录 %s 第 %d 页反复返回空（total=%d），疑似被限流，已放弃而非当作空目录",
				parentID, page, resp.Data.Total)
		}
		if resp.Data.List == nil {
			break // 正常翻完
		}
		all = append(all, *resp.Data.List...)
		if len(*resp.Data.List) < pageSize {
			break
		}
		if resp.Data.Total > 0 && len(all) >= resp.Data.Total {
			break
		}
	}
	g.mu.Lock()
	g.listCache[parentID] = all
	g.mu.Unlock()
	return all, nil
}

// resolveID 把斜杠路径解析成 fileId；notFound 时返回空 ID 且 ok=false
func (g *GuangYaBackend) resolveID(ctx context.Context, p string) (string, bool, error) {
	clean := strings.Trim(strings.TrimSpace(p), "/")
	if clean == "" {
		return "", true, nil // 根
	}
	g.mu.Lock()
	if id, ok := g.idCache[clean]; ok {
		g.mu.Unlock()
		return id, true, nil
	}
	g.mu.Unlock()

	parentID := ""
	var acc []string
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" {
			continue
		}
		acc = append(acc, seg)
		key := strings.Join(acc, "/")

		g.mu.Lock()
		cached, hit := g.idCache[key]
		g.mu.Unlock()
		if hit {
			parentID = cached
			continue
		}

		items, err := g.listDir(ctx, parentID, false)
		if err != nil {
			return "", false, err
		}
		found := ""
		for _, it := range items {
			if it.FileName == seg {
				found = it.FileID
				break
			}
		}
		if found == "" {
			return "", false, nil
		}
		g.mu.Lock()
		g.idCache[key] = found
		g.mu.Unlock()
		parentID = found
	}
	return parentID, true, nil
}

func (g *GuangYaBackend) Walk(ctx context.Context, root string, fn func(rel string) error) error {
	rootID, ok, err := g.resolveID(ctx, root)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("光鸭路径不存在: %s", root)
	}

	type node struct {
		id  string
		rel string
	}
	queue := []node{{rootID, ""}}
	for len(queue) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		cur := queue[0]
		queue = queue[1:]

		items, err := g.listDir(ctx, cur.id, false)
		if err != nil {
			return err
		}
		for _, it := range items {
			rel := it.FileName
			if cur.rel != "" {
				rel = cur.rel + "/" + it.FileName
			}
			if it.IsDir() {
				queue = append(queue, node{it.FileID, rel})
				continue
			}
			if strings.HasPrefix(it.FileName, ".") {
				continue
			}
			if err := fn(rel); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *GuangYaBackend) MkdirAll(ctx context.Context, dir string) error {
	clean := strings.Trim(strings.TrimSpace(dir), "/")
	if clean == "" {
		return nil
	}
	parentID := ""
	var acc []string
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" {
			continue
		}
		acc = append(acc, seg)
		key := strings.Join(acc, "/")

		g.mu.Lock()
		cached, hit := g.idCache[key]
		g.mu.Unlock()
		if hit {
			parentID = cached
			continue
		}

		// 先查是否已存在（幂等）
		items, err := g.listDir(ctx, parentID, false)
		if err != nil {
			return err
		}
		found := ""
		for _, it := range items {
			if it.FileName == seg && it.IsDir() {
				found = it.FileID
				break
			}
		}
		if found == "" {
			var out struct {
				Data struct {
					FileID string `json:"fileId"`
				} `json:"data"`
			}
			if err := g.post(ctx, "/nd.bizuserres.s/v1/file/create_dir", map[string]any{
				"parentId": parentID,
				"dirName":  seg,
			}, &out); err != nil {
				return fmt.Errorf("创建目录 %s 失败: %w", key, err)
			}
			found = out.Data.FileID
			// 父目录列表已变
			g.mu.Lock()
			delete(g.listCache, parentID)
			g.mu.Unlock()
		}
		g.mu.Lock()
		g.idCache[key] = found
		g.mu.Unlock()
		parentID = found
	}
	return nil
}

func (g *GuangYaBackend) Exists(ctx context.Context, p string) (bool, error) {
	_, ok, err := g.resolveID(ctx, p)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// Rename 改名 + 必要时移动。
//
// 与 Alist 后端同样的两步顺序：先在源目录改成规范名，再移动。
// 理由相同 —— 移动到已有同名文件的目录时行为不确定。
func (g *GuangYaBackend) Rename(ctx context.Context, src, dst string) error {
	srcDir, srcName := path.Split(src)
	dstDir, dstName := path.Split(dst)
	srcDir = strings.TrimRight(srcDir, "/")
	dstDir = strings.TrimRight(dstDir, "/")

	fileID, ok, err := g.resolveID(ctx, src)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("源文件不存在: %s", src)
	}

	// 第一步：改名
	if srcName != dstName {
		if err := g.post(ctx, "/nd.bizuserres.s/v1/file/rename", map[string]any{
			"fileId":  fileID,
			"newName": dstName,
		}, nil); err != nil {
			return fmt.Errorf("改名失败: %w", err)
		}
		g.mu.Lock()
		delete(g.idCache, strings.Trim(src, "/"))
		g.mu.Unlock()
	}
	g.invalidate(srcDir)

	if srcDir == dstDir {
		return nil
	}

	// 第二步：移动
	dstID, ok, err := g.resolveID(ctx, dstDir)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("目标目录不存在: %s（应先 MkdirAll）", dstDir)
	}

	var out struct {
		Data struct {
			TaskID string `json:"taskId"`
		} `json:"data"`
	}
	if err := g.post(ctx, "/nd.bizuserres.s/v1/file/move_file", map[string]any{
		"fileIds":  []string{fileID},
		"parentId": dstID,
	}, &out); err != nil {
		return fmt.Errorf("移动失败（文件现名 %s，位于 %s）: %w", dstName, srcDir, err)
	}
	g.invalidate(dstDir)

	if tid := strings.TrimSpace(out.Data.TaskID); tid != "" {
		return g.waitTask(ctx, tid)
	}
	return nil
}

// waitTask 轮询异步任务；status 2=完成 -1/3=失败
func (g *GuangYaBackend) waitTask(ctx context.Context, taskID string) error {
	const maxTry = 30
	for i := 0; i < maxTry; i++ {
		var out struct {
			Data struct {
				Status int `json:"status"`
			} `json:"data"`
		}
		if err := g.post(ctx, "/nd.bizuserres.s/v1/get_task_status", map[string]any{
			"taskId": taskID,
		}, &out); err != nil {
			return err
		}
		switch out.Data.Status {
		case 2:
			return nil
		case -1, 3:
			return fmt.Errorf("光鸭任务 %s 失败 status=%d", taskID, out.Data.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("光鸭任务 %s 超时", taskID)
}

func (g *GuangYaBackend) invalidate(dir string) {
	id, _, err := g.resolveID(context.Background(), dir)
	if err != nil {
		return
	}
	g.mu.Lock()
	delete(g.listCache, id)
	g.mu.Unlock()
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
