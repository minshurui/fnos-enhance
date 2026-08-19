package transfer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fnos-enhance/internal/linker"
)

// 说明：本文件用 httptest 模拟网盘响应，验证的是
//   分页收敛 / 递归遍历 / Cookie 携带 / 凭据校验 / 重试
// 这些是审计里真实存在的结构性 bug 的回归测试。
// 它 **不能** 证明与真实网盘 API 的字段兼容性——那需要真实凭据端到端跑。

// ------------------------------------------------------------
// Config 校验（审计 P0-6 根因：多个同类型 string 参数错位）
// ------------------------------------------------------------

func TestConfigValidate_RejectsEmpty(t *testing.T) {
	var c Config
	// Validate 只填默认值，不再硬拦空凭据：
	// 实测夸克列举接口无 cookie 也能读公开分享，dry-run 预览应免凭据
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate 不应因空凭据报错: %v", err)
	}
	// 但真转存（写入）前必须报错
	if err := c.RequireCredentials(); err == nil {
		t.Fatal("三家凭据全空时，真转存必须报错")
	}
}

func TestConfigValidate_FillsDefaults(t *testing.T) {
	c := Config{Quark: QuarkConfig{Cookie: "x"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Timeout == 0 || c.MaxRetries == 0 || c.PageSize == 0 || c.MaxDepth == 0 {
		t.Errorf("默认值未填充: %+v", c)
	}
}

func TestNew_MissingProviderReportsClearly(t *testing.T) {
	// 只配夸克，却来了百度链接 → 必须明确报"未配置"，不能 panic 也不能静默成功
	tr, err := New(Config{Quark: QuarkConfig{Cookie: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.Transfer(context.Background(),
		linker.ShareLink{Type: linker.LinkBaidu, ID: "1abc", Link: "x"}, true)
	if err == nil || !strings.Contains(err.Error(), "百度凭据未配置") {
		t.Errorf("期望明确的凭据未配置错误，得到: %v", err)
	}
}

// 审计 P0-6 回归：光鸭只给 ClientID 不给 token 时，
// 旧代码构造成功但 Transfer 永远失败；现在构造期就拦住
func TestGuangYa_ClientIDAloneIsNotReady(t *testing.T) {
	c := GuangYaConfig{ClientID: "cid"} // 只有 ClientID，没 refresh_token
	if c.Ready() {
		t.Error("只有 ClientID 不应算就绪——这正是旧代码 100% 失败的根因")
	}
	cfg := Config{GuangYa: c}
	if err := cfg.RequireCredentials(); err == nil {
		t.Error("只有 ClientID 时，真转存应报错")
	}
}

// 实测发现：夸克 token/detail 无 cookie 也能读公开分享
// → dry-run 预览不得因缺 cookie 而失败，但 --execute 必须拦住
func TestQuark_DryRunWorksWithoutCookie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/token"):
			writeJSON(w, map[string]interface{}{"status": 200, "data": map[string]string{"stoken": "ST"}})
		case strings.Contains(r.URL.Path, "/detail"):
			writeJSON(w, map[string]interface{}{
				"status": 200,
				"data": map[string]interface{}{"list": []map[string]interface{}{
					{"fid": "F1", "file_name": "01.4k.mp4", "file_type": 1},
				}},
				"metadata": map[string]interface{}{"_total": 1},
			})
		case strings.Contains(r.URL.Path, "/save"):
			t.Error("dry-run 不应调 save")
		}
	}))
	defer srv.Close()

	q := newTestQuark(srv.URL)
	q.cfg.Cookie = "" // 无凭据

	res, err := q.Transfer(context.Background(), linker.ShareLink{ID: "abc"}, true)
	if err != nil {
		t.Fatalf("无 cookie 的 dry-run 列举应成功（真实 API 已验证）: %v", err)
	}
	if len(res.Names) != 1 {
		t.Errorf("列举结果不对: %+v", res.Names)
	}

	// 但 execute 必须报错
	_, err = q.Transfer(context.Background(), linker.ShareLink{ID: "abc"}, false)
	if err == nil || !strings.Contains(err.Error(), "QUARK_COOKIE") {
		t.Errorf("无 cookie 的真转存必须报错并指向 QUARK_COOKIE，得到: %v", err)
	}
}

func TestGuangYa_RefreshPairIsReady(t *testing.T) {
	c := GuangYaConfig{ClientID: "cid", RefreshToken: "rt"}
	if !c.Ready() {
		t.Error("ClientID + RefreshToken 应可用（能刷新出 access_token）")
	}
}

// ------------------------------------------------------------
// 夸克：分页 + 递归（审计 P0-4：旧代码 _size=50 单页不递归）
// ------------------------------------------------------------

func TestQuark_PaginationCollectsAllPages(t *testing.T) {
	const total = 120 // 超过单页 50，需翻 3 页
	var savedFIDs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/token"):
			writeJSON(w, map[string]interface{}{
				"status": 200, "data": map[string]string{"stoken": "ST"},
			})
		case strings.Contains(r.URL.Path, "/detail"):
			page := atoiDefault(r.URL.Query().Get("_page"), 1)
			size := atoiDefault(r.URL.Query().Get("_size"), 50)
			var list []map[string]interface{}
			for i := (page - 1) * size; i < page*size && i < total; i++ {
				list = append(list, map[string]interface{}{
					"fid": fmt.Sprintf("f%03d", i), "file_name": fmt.Sprintf("S01E%03d.mp4", i+1),
					"share_fid_token": fmt.Sprintf("t%03d", i), "dir": false, "file_type": 1,
				})
			}
			writeJSON(w, map[string]interface{}{
				"status": 200,
				"data":   map[string]interface{}{"list": list},
				"metadata": map[string]interface{}{
					"_total": total, "_page": page, "_size": size,
				},
			})
		case strings.Contains(r.URL.Path, "/save"):
			var body struct {
				FidList []string `json:"fid_list"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			savedFIDs = body.FidList
			writeJSON(w, map[string]interface{}{"status": 200, "data": map[string]string{"task_id": "T1"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	q := newTestQuark(srv.URL)
	res, err := q.Transfer(context.Background(),
		linker.ShareLink{Type: linker.LinkQuark, ID: "abc", Link: "https://pan.quark.cn/s/abc"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Names) != total {
		t.Errorf("分页未取全: 期望 %d 条，得到 %d（旧代码只会拿到 50）", total, len(res.Names))
	}
	if len(savedFIDs) != total {
		t.Errorf("提交转存的 fid 数不对: 期望 %d，得到 %d", total, len(savedFIDs))
	}
	if res.Transferred != total {
		t.Errorf("Transferred=%d，期望 %d", res.Transferred, total)
	}
}

func TestQuark_RecursesIntoSubdirs(t *testing.T) {
	// 顶层 1 个目录，目录内 2 个文件 → 递归后应有 3 条 Entry
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/token"):
			writeJSON(w, map[string]interface{}{"status": 200, "data": map[string]string{"stoken": "ST"}})
		case strings.Contains(r.URL.Path, "/detail"):
			pdir := r.URL.Query().Get("pdir_fid")
			if pdir == "0" {
				writeJSON(w, map[string]interface{}{
					"status": 200,
					"data": map[string]interface{}{"list": []map[string]interface{}{
						{"fid": "DIR1", "file_name": "狂飙 S01 全39集", "dir": true, "file_type": 0, "share_fid_token": "td"},
					}},
					"metadata": map[string]interface{}{"_total": 1},
				})
				return
			}
			writeJSON(w, map[string]interface{}{
				"status": 200,
				"data": map[string]interface{}{"list": []map[string]interface{}{
					{"fid": "F1", "file_name": "S01E01.mp4", "dir": false, "file_type": 1},
					{"fid": "F2", "file_name": "S01E02.mp4", "dir": false, "file_type": 1},
				}},
				"metadata": map[string]interface{}{"_total": 2},
			})
		case strings.Contains(r.URL.Path, "/save"):
			writeJSON(w, map[string]interface{}{"status": 200})
		}
	}))
	defer srv.Close()

	q := newTestQuark(srv.URL)
	res, err := q.Transfer(context.Background(),
		linker.ShareLink{Type: linker.LinkQuark, ID: "abc"}, false)
	if err != nil {
		t.Fatal(err)
	}
	// 顶层只有 1 个目录条目（转存时提交目录 ID，网盘保留内部结构）
	if len(res.Names) != 1 || res.Names[0] != "狂飙 S01 全39集" {
		t.Errorf("顶层条目不对: %v", res.Names)
	}
	// 递归后应看到目录内的文件
	if res.FileCount() != 2 {
		t.Errorf("递归文件数: 期望 2，得到 %d（旧代码不递归=0）", res.FileCount())
	}
	if len(res.Entries) != 3 {
		t.Errorf("总条目数: 期望 3(1目录+2文件)，得到 %d", len(res.Entries))
	}
}

func TestQuark_DryRunDoesNotSave(t *testing.T) {
	var saveCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/token"):
			writeJSON(w, map[string]interface{}{"status": 200, "data": map[string]string{"stoken": "ST"}})
		case strings.Contains(r.URL.Path, "/detail"):
			writeJSON(w, map[string]interface{}{
				"status": 200,
				"data": map[string]interface{}{"list": []map[string]interface{}{
					{"fid": "F1", "file_name": "a.mp4", "file_type": 1},
				}},
				"metadata": map[string]interface{}{"_total": 1},
			})
		case strings.Contains(r.URL.Path, "/save"):
			saveCalled.Store(true)
			writeJSON(w, map[string]interface{}{"status": 200})
		}
	}))
	defer srv.Close()

	q := newTestQuark(srv.URL)
	res, err := q.Transfer(context.Background(), linker.ShareLink{ID: "abc"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if saveCalled.Load() {
		t.Error("dry-run 绝不能调 save")
	}
	if !res.DryRun || res.Transferred != 0 {
		t.Errorf("dry-run 结果标记不对: %+v", res)
	}
}

func TestQuark_ShareUnavailableReportsClearly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{"status": 400, "message": "分享已失效"})
	}))
	defer srv.Close()

	q := newTestQuark(srv.URL)
	_, err := q.Transfer(context.Background(), linker.ShareLink{ID: "abc"}, true)
	if err == nil || !strings.Contains(err.Error(), "不可访问") {
		t.Errorf("失效分享应明确报错，得到: %v", err)
	}
}

// ------------------------------------------------------------
// 重试（审计 P1-7）
// ------------------------------------------------------------

func TestRetry_On429ThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"errno":-55}`))
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	}))
	defer srv.Close()

	var out map[string]interface{}
	// MaxRetries=2，退避从 1s 起，测试可接受
	err := doJSON(context.Background(), srv.Client(), 2, http.MethodGet, srv.URL, nil, nil, &out)
	if err != nil {
		t.Fatalf("429 后应重试成功: %v", err)
	}
	if hits.Load() < 2 {
		t.Errorf("未发生重试，hits=%d", hits.Load())
	}
}

func TestRetry_ContextCancelStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var out map[string]interface{}
	err := doJSON(ctx, srv.Client(), 5, http.MethodGet, srv.URL, nil, nil, &out)
	if err == nil {
		t.Fatal("应因 context 取消而返回错误")
	}
}

func TestHTTPError_RetryableClassification(t *testing.T) {
	cases := map[int]bool{429: true, 408: true, 500: true, 503: true, 400: false, 403: false, 404: false}
	for code, want := range cases {
		he := &httpError{Code: code}
		if he.retryable() != want {
			t.Errorf("HTTP %d retryable=%v, want %v", code, he.retryable(), want)
		}
	}
}

// ------------------------------------------------------------
// 百度：CookieJar（审计 P1-1）+ bdstoken（P1-2）+ 分页
// ------------------------------------------------------------

func TestBaidu_CarriesBDCLNDAfterVerify(t *testing.T) {
	var listSawBDCLND atomic.Bool
	var listSawBDUSS atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/s/"):
			// 分享页：给 shareid/uk/bdstoken
			w.Write([]byte(`<script>var x={"shareid":12345,"uk":67890,"bdstoken":"0123456789abcdef0123456789abcdef"}</script>`))
		case r.URL.Path == "/share/verify":
			// 验证成功并下发 BDCLND
			http.SetCookie(w, &http.Cookie{Name: "BDCLND", Value: "RANDSK123", Path: "/"})
			writeJSON(w, map[string]interface{}{"errno": 0, "randsk": "RANDSK123"})
		case r.URL.Path == "/share/list":
			for _, c := range r.Cookies() {
				if c.Name == "BDCLND" {
					listSawBDCLND.Store(true)
				}
				if c.Name == "BDUSS" {
					listSawBDUSS.Store(true)
				}
			}
			writeJSON(w, map[string]interface{}{"errno": 0, "list": []map[string]interface{}{
				{"fs_id": 111, "server_filename": "花开锦秀", "isdir": 1, "path": "/花开锦秀"},
			}})
		case r.URL.Path == "/share/transfer":
			writeJSON(w, map[string]interface{}{"errno": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b := newTestBaidu(srv.URL)
	_, err := b.Transfer(context.Background(),
		linker.ShareLink{Type: linker.LinkBaidu, ID: "1abcdef", Pwd: "abcd"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !listSawBDCLND.Load() {
		t.Error("verify 后的 BDCLND 未带到 list 请求——这正是审计 P1-1（旧代码 Jar==nil，带提取码分享 100% 失败）")
	}
	if !listSawBDUSS.Load() {
		t.Error("静态 BDUSS 凭据丢失")
	}
}

func TestBaidu_ExtractsRealBDSToken(t *testing.T) {
	const want = "0123456789abcdef0123456789abcdef"
	var listToken string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/s/"):
			fmt.Fprintf(w, `{"shareid":1,"uk":2,"bdstoken":"%s"}`, want)
		case r.URL.Path == "/share/list":
			listToken = r.URL.Query().Get("bdstoken")
			writeJSON(w, map[string]interface{}{"errno": 0, "list": []map[string]interface{}{
				{"fs_id": 1, "server_filename": "a.mp4", "isdir": 0},
			}})
		case r.URL.Path == "/share/transfer":
			writeJSON(w, map[string]interface{}{"errno": 0})
		}
	}))
	defer srv.Close()

	b := newTestBaidu(srv.URL)
	if _, err := b.Transfer(context.Background(), linker.ShareLink{ID: "1abc"}, false); err != nil {
		t.Fatal(err)
	}
	if listToken != want {
		t.Errorf("bdstoken 未从页面提取: 得到 %q，期望 %q（旧代码硬写 null）", listToken, want)
	}
	if listToken == "null" {
		t.Error("仍在使用硬编码 null")
	}
}

func TestBaidu_PaginationAndRecursion(t *testing.T) {
	const perPage = 100
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/s/"):
			w.Write([]byte(`{"shareid":1,"uk":2,"bdstoken":"0123456789abcdef0123456789abcdef"}`))
		case r.URL.Path == "/share/list":
			page := atoiDefault(r.URL.Query().Get("page"), 1)
			dir := r.URL.Query().Get("dir")
			if dir == "" { // 根目录：2 页，第 2 页只有 1 条目录
				if page == 1 {
					var list []map[string]interface{}
					for i := 0; i < perPage; i++ {
						list = append(list, map[string]interface{}{
							"fs_id": 1000 + i, "server_filename": fmt.Sprintf("m%03d.mp4", i), "isdir": 0,
						})
					}
					writeJSON(w, map[string]interface{}{"errno": 0, "list": list})
					return
				}
				if page == 2 {
					writeJSON(w, map[string]interface{}{"errno": 0, "list": []map[string]interface{}{
						{"fs_id": 9999, "server_filename": "剧集", "isdir": 1, "path": "/剧集"},
					}})
					return
				}
				writeJSON(w, map[string]interface{}{"errno": 0, "list": []map[string]interface{}{}})
				return
			}
			// 子目录
			writeJSON(w, map[string]interface{}{"errno": 0, "list": []map[string]interface{}{
				{"fs_id": 7001, "server_filename": "S01E01.mp4", "isdir": 0},
			}})
		case r.URL.Path == "/share/transfer":
			writeJSON(w, map[string]interface{}{"errno": 0})
		}
	}))
	defer srv.Close()

	b := newTestBaidu(srv.URL)
	res, err := b.Transfer(context.Background(), linker.ShareLink{ID: "1abc"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Names) != perPage+1 {
		t.Errorf("顶层分页未取全: 期望 %d，得到 %d", perPage+1, len(res.Names))
	}
	// 100 个顶层文件 + 1 个目录内的文件
	if res.FileCount() != perPage+1 {
		t.Errorf("递归文件数: 期望 %d，得到 %d", perPage+1, res.FileCount())
	}
}

func TestBaidu_WrongPasswordReportsClearly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/s/"):
			w.Write([]byte(`{"shareid":1,"uk":2}`))
		case r.URL.Path == "/share/verify":
			writeJSON(w, map[string]interface{}{"errno": -9})
		}
	}))
	defer srv.Close()

	b := newTestBaidu(srv.URL)
	_, err := b.Transfer(context.Background(), linker.ShareLink{ID: "1abc", Pwd: "wrong"}, true)
	if err == nil || !strings.Contains(err.Error(), "-9") {
		t.Errorf("错误提取码应明确报 errno=-9，得到: %v", err)
	}
}

func TestBaidu_DeadLinkReportsClearly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>分享已取消</html>`))
	}))
	defer srv.Close()

	b := newTestBaidu(srv.URL)
	_, err := b.Transfer(context.Background(), linker.ShareLink{ID: "1abc"}, true)
	if err == nil || !strings.Contains(err.Error(), "shareid") {
		t.Errorf("失效链接应明确报错，得到: %v", err)
	}
}

// ------------------------------------------------------------
// 光鸭：JWT 过期 + 自动刷新（审计 P0-6）
// ------------------------------------------------------------

func TestJWTExpired(t *testing.T) {
	past := makeJWT(time.Now().Add(-time.Hour).Unix())
	future := makeJWT(time.Now().Add(time.Hour).Unix())

	if !jwtExpired(past) {
		t.Error("过期 JWT 应判为过期")
	}
	if jwtExpired(future) {
		t.Error("未过期 JWT 不应判为过期")
	}
	if jwtExpired("not-a-jwt") {
		t.Error("非 JWT 格式应交给服务端判断（不判过期）")
	}
	// 刚好在 60s 缓冲区内 → 视为过期，避免请求途中失效
	if !jwtExpired(makeJWT(time.Now().Add(30 * time.Second).Unix())) {
		t.Error("30s 内到期应提前判为过期")
	}
}

func TestGuangYa_AutoRefreshesExpiredToken(t *testing.T) {
	expired := makeJWT(time.Now().Add(-time.Hour).Unix())
	fresh := makeJWT(time.Now().Add(time.Hour).Unix())
	var refreshCalled atomic.Bool
	var restoreAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/token":
			refreshCalled.Store(true)
			writeJSON(w, map[string]interface{}{"access_token": fresh})
		case "/userres/v1/get_share_access_token":
			writeJSON(w, map[string]interface{}{"code": 0, "data": map[string]string{"accessToken": "SAT"}})
		case "/userres/v1/get_share_page_files_list":
			writeJSON(w, map[string]interface{}{"code": 0, "data": map[string]interface{}{
				"fileList": []map[string]interface{}{
					{"fileId": "F1", "fileName": "a.mp4", "isDir": false, "fileType": 1},
				},
				"total": 1,
			}})
		case "/userres/v1/restore_share":
			restoreAuth = r.Header.Get("Authorization")
			writeJSON(w, map[string]interface{}{"code": 0, "msg": "success"})
		}
	}))
	defer srv.Close()

	g := &GuangYaTransferor{
		cfg:     GuangYaConfig{AccessToken: expired, RefreshToken: "RT", ClientID: "CID"},
		common:  Config{MaxRetries: 0, PageSize: 100, MaxDepth: 2, Timeout: 5 * time.Second},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		apiBase: srv.URL,
		authURL: srv.URL + "/v1/auth/token",
	}
	res, err := g.Transfer(context.Background(), linker.ShareLink{ID: "sid"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshCalled.Load() {
		t.Error("过期 token 应触发自动刷新")
	}
	if restoreAuth != "Bearer "+fresh {
		t.Errorf("转存应使用刷新后的 token，得到: %q", restoreAuth)
	}
	if res.Transferred != 1 {
		t.Errorf("Transferred=%d，期望 1", res.Transferred)
	}
}

func TestGuangYa_RefreshFailureReportsClearly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{"error": "invalid_grant", "error_description": "refresh token expired"})
	}))
	defer srv.Close()

	g := &GuangYaTransferor{
		cfg:     GuangYaConfig{RefreshToken: "RT", ClientID: "CID"},
		common:  Config{MaxRetries: 0, PageSize: 100, MaxDepth: 1, Timeout: 5 * time.Second},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		apiBase: srv.URL,
		authURL: srv.URL + "/v1/auth/token",
	}
	_, err := g.Transfer(context.Background(), linker.ShareLink{ID: "sid"}, true)
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("刷新失败应透传原因，得到: %v", err)
	}
}

func TestGuangYa_PaginationAndRecursion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/userres/v1/get_share_access_token":
			writeJSON(w, map[string]interface{}{"code": 0, "data": map[string]string{"accessToken": "SAT"}})
		case "/userres/v1/get_share_page_files_list":
			var body struct {
				ParentID string `json:"parentId"`
				PageNum  int    `json:"pageNum"`
				PageSize int    `json:"pageSize"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.ParentID == "" {
				if body.PageNum == 1 {
					var list []map[string]interface{}
					for i := 0; i < body.PageSize; i++ {
						list = append(list, map[string]interface{}{
							"fileId": fmt.Sprintf("F%03d", i), "fileName": fmt.Sprintf("a%03d.mp4", i),
							"isDir": false, "fileType": 1,
						})
					}
					writeJSON(w, map[string]interface{}{"code": 0,
						"data": map[string]interface{}{"fileList": list}})
					return
				}
				if body.PageNum == 2 {
					writeJSON(w, map[string]interface{}{"code": 0, "data": map[string]interface{}{
						"fileList": []map[string]interface{}{
							{"fileId": "D1", "fileName": "第二季", "isDir": true, "fileType": 0},
						}}})
					return
				}
				writeJSON(w, map[string]interface{}{"code": 0, "data": map[string]interface{}{"fileList": []interface{}{}}})
				return
			}
			writeJSON(w, map[string]interface{}{"code": 0, "data": map[string]interface{}{
				"fileList": []map[string]interface{}{
					{"fileId": "S1", "fileName": "S02E01.mp4", "isDir": false, "fileType": 1},
				}}})
		case "/userres/v1/restore_share":
			writeJSON(w, map[string]interface{}{"code": 0, "msg": "success"})
		}
	}))
	defer srv.Close()

	g := &GuangYaTransferor{
		cfg:     GuangYaConfig{AccessToken: makeJWT(time.Now().Add(time.Hour).Unix())},
		common:  Config{MaxRetries: 0, PageSize: 100, MaxDepth: 2, Timeout: 5 * time.Second},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		apiBase: srv.URL,
	}
	res, err := g.Transfer(context.Background(), linker.ShareLink{ID: "sid"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Names) != 101 {
		t.Errorf("顶层分页未取全: 期望 101，得到 %d", len(res.Names))
	}
	if res.FileCount() != 101 {
		t.Errorf("递归文件数: 期望 101(100顶层+1子目录)，得到 %d", res.FileCount())
	}
}

// ------------------------------------------------------------
// 辅助
// ------------------------------------------------------------

func newTestQuark(base string) *QuarkTransferor {
	return &QuarkTransferor{
		cfg:     QuarkConfig{Cookie: "__puus=test"},
		common:  Config{MaxRetries: 0, PageSize: 50, MaxDepth: 2, Timeout: 5 * time.Second},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		baseURL: base,
	}
}

func newTestBaidu(base string) *BaiduTransferor {
	return &BaiduTransferor{
		cfg:     BaiduConfig{Cookie: "BDUSS=test"},
		common:  Config{MaxRetries: 0, PageSize: 100, MaxDepth: 2, Timeout: 5 * time.Second},
		HTTP:    newHTTPClient(5*time.Second, true),
		baseURL: base,
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func atoiDefault(s string, def int) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}

// makeJWT 造一个只含 exp 的假 JWT（测试用）
func makeJWT(exp int64) string {
	payload := fmt.Sprintf(`{"exp":%d}`, exp)
	return "h." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".s"
}
