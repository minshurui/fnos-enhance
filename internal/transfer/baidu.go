package transfer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"fnos-enhance/internal/linker"
)

// BaiduTransferor 百度网盘转存器
//
// 与旧实现的差异（审计 P1-1 / P1-2）：
//   - HTTP.Jar 必须非 nil：verify 成功后百度下发 BDCLND cookie，
//     后续 list/transfer 必须带上，否则带提取码的分享 100% 失败
//   - bdstoken 从分享页/主页真实抓取，不再硬写 "null"
type BaiduTransferor struct {
	cfg    BaiduConfig
	common Config
	HTTP   *http.Client

	bdstoken string
	// baseURL 仅供测试注入，空则用真实域名
	baseURL string
}

func (b *BaiduTransferor) base() string {
	if b.baseURL != "" {
		return b.baseURL
	}
	return "https://pan.baidu.com"
}

var (
	reShareID  = regexp.MustCompile(`"shareid"\s*[:=]\s*"?(\d+)"?`)
	reUK       = regexp.MustCompile(`"uk"\s*[:=]\s*"?(\d+)"?`)
	reBDSToken = regexp.MustCompile(`"bdstoken"\s*:\s*"([0-9a-f]{32})"`)
)

type baiduListResp struct {
	Errno int `json:"errno"`
	List  []struct {
		FSID     json.Number `json:"fs_id"`
		Filename string      `json:"server_filename"`
		IsDir    json.Number `json:"isdir"`
		Size     json.Number `json:"size"`
		Path     string      `json:"path"`
	} `json:"list"`
}

func (b *BaiduTransferor) headers(referer string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("User-Agent", browserUA)
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		// Jar 管理动态 cookie（BDCLND），静态凭据（BDUSS）仍需显式带
		if b.cfg.Cookie != "" {
			if existing := req.Header.Get("Cookie"); existing != "" {
				req.Header.Set("Cookie", b.cfg.Cookie+"; "+existing)
			} else {
				req.Header.Set("Cookie", b.cfg.Cookie)
			}
		}
	}
}

// Transfer 百度分享转存：分享页 → verify → list(分页+递归) → transfer
func (b *BaiduTransferor) Transfer(ctx context.Context, link linker.ShareLink, dryRun bool) (*TransferResult, error) {
	if link.ID == "" {
		return nil, fmt.Errorf("无法解析百度链接: %s", link.Link)
	}
	if b.cfg.Cookie == "" {
		return nil, fmt.Errorf("百度 cookie 未配置")
	}
	surl, pwd := link.ID, link.Pwd

	// 1. 访问分享页拿 shareid / uk / bdstoken
	pageURL := b.base() + "/s/" + surl
	if pwd != "" {
		pageURL += "?pwd=" + url.QueryEscape(pwd)
	}
	body, err := doForm(ctx, b.HTTP, b.common.MaxRetries, http.MethodGet, pageURL, "",
		b.headers(b.base()+"/disk/home"))
	if err != nil {
		return nil, fmt.Errorf("百度访问分享页失败: %w", err)
	}
	html := string(body)

	sid := firstGroup(reShareID, html)
	ukv := firstGroup(reUK, html)
	if sid == "" || ukv == "" {
		return nil, fmt.Errorf("百度分享页无 shareid/uk：链接可能已失效、被和谐或需要登录")
	}
	if tok := firstGroup(reBDSToken, html); tok != "" {
		b.bdstoken = tok
	}

	// 2. 验证提取码（成功后 Jar 会保存 BDCLND）
	if pwd != "" {
		vURL := fmt.Sprintf(b.base()+"/share/verify?surl=%s&t=%d&channel=chunlei&web=1&app_id=250528&bdstoken=%s&clienttype=0",
			strings.TrimPrefix(surl, "1"), time.Now().UnixMilli(), b.tokenOrEmpty())
		form := "pwd=" + url.QueryEscape(pwd) + "&vcode=&vcode_str="
		vbuf, err := doForm(ctx, b.HTTP, b.common.MaxRetries, http.MethodPost, vURL, form,
			b.headers(b.base()+"/s/"+surl))
		if err != nil {
			return nil, fmt.Errorf("百度提取码验证请求失败: %w", err)
		}
		var vr struct {
			Errno  int    `json:"errno"`
			Randsk string `json:"randsk"`
		}
		if err := json.Unmarshal(vbuf, &vr); err != nil {
			return nil, fmt.Errorf("百度提取码响应解析失败: %s", truncate(string(vbuf), 120))
		}
		if vr.Errno != 0 {
			return nil, fmt.Errorf("百度提取码验证失败 errno=%d（-9=提取码错误, -12=尝试过多）", vr.Errno)
		}
		// randsk 就是 BDCLND 的值；Jar 未捕获时手工兜底写入
		if vr.Randsk != "" {
			b.ensureBDCLND(vr.Randsk)
		}
		// 再访问一次分享页，让服务端确认已授权
		_, _ = doForm(ctx, b.HTTP, b.common.MaxRetries, http.MethodGet,
			b.base()+"/s/"+surl, "",
			b.headers(b.base()+"/share/init?surl="+surl))
	}

	// 3. 列举顶层 + 递归（审计 P0-4：旧代码 root=1&num=100 单页不递归）
	top, err := b.listDir(ctx, sid, ukv, surl, "/", 0)
	if err != nil {
		return nil, err
	}
	if len(top) == 0 {
		return nil, fmt.Errorf("分享无文件")
	}

	all := append([]Entry(nil), top...)
	if b.common.MaxDepth > 0 {
		sub, err := b.walk(ctx, sid, ukv, surl, top, 1)
		if err != nil {
			fmt.Printf("警告: 百度递归列举中断（%v），仅按顶层条目转存\n", err)
		} else {
			all = append(all, sub...)
		}
	}

	names := make([]string, 0, len(top))
	fsIDs := make([]string, 0, len(top))
	for _, e := range top {
		names = append(names, e.Name)
		fsIDs = append(fsIDs, e.ID)
	}

	result := &TransferResult{
		Provider: "百度", Link: link.Link,
		Names: names, Entries: all, DryRun: dryRun,
	}
	if dryRun {
		return result, nil
	}

	// 4. 转存
	toDir := b.cfg.ToDir
	if toDir == "" {
		toDir = "/"
	}
	tURL := fmt.Sprintf(b.base()+"/share/transfer?shareid=%s&from=%s&bdstoken=%s"+
		"&channel=chunlei&clienttype=0&web=1&app_id=250528", sid, ukv, b.tokenOrEmpty())
	form := "fsidlist=[" + strings.Join(fsIDs, ",") + "]&path=" + url.QueryEscape(toDir) + "&async=1&ondup=newcopy"
	tbuf, err := doForm(ctx, b.HTTP, b.common.MaxRetries, http.MethodPost, tURL, form,
		b.headers(b.base()+"/s/"+surl))
	if err != nil {
		return nil, fmt.Errorf("百度转存请求失败: %w", err)
	}
	var tr struct {
		Errno int `json:"errno"`
		Info  []struct {
			Errno int `json:"errno"`
		} `json:"info"`
	}
	if err := json.Unmarshal(tbuf, &tr); err != nil {
		return nil, fmt.Errorf("百度转存响应解析失败: %s", truncate(string(tbuf), 120))
	}
	// 4 = 文件已存在，视为成功（幂等）
	if tr.Errno != 0 && tr.Errno != 4 {
		return nil, fmt.Errorf("百度转存被拒 errno=%d（-8=已存在同名, 12=部分失败, 2=参数错误, -6=cookie失效）", tr.Errno)
	}

	result.Transferred = len(fsIDs)
	return result, nil
}

// listDir 列举分享内某目录（自动翻页）
func (b *BaiduTransferor) listDir(ctx context.Context, sid, ukv, surl, dir string, depth int) ([]Entry, error) {
	num := b.common.PageSize
	if num <= 0 || num > 1000 {
		num = 100
	}
	shorturl := strings.TrimPrefix(surl, "1")

	var out []Entry
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		u := fmt.Sprintf(b.base()+"/share/list?shareid=%s&uk=%s&shorturl=%s"+
			"&page=%d&num=%d&order=name&desc=0&showempty=0&web=5&app_id=250528&channel=chunlei&clienttype=0&bdstoken=%s",
			sid, ukv, url.QueryEscape(shorturl), page, num, b.tokenOrEmpty())
		if dir == "/" || dir == "" {
			u += "&root=1"
		} else {
			u += "&dir=" + url.QueryEscape(dir)
		}

		buf, err := doForm(ctx, b.HTTP, b.common.MaxRetries, http.MethodGet, u, "",
			b.headers(b.base()+"/share/init?surl="+surl))
		if err != nil {
			return out, fmt.Errorf("百度列举 %s 第 %d 页失败: %w", dir, page, err)
		}
		var lr baiduListResp
		if err := json.Unmarshal(buf, &lr); err != nil {
			return out, fmt.Errorf("百度列表解析失败: %s", truncate(string(buf), 120))
		}
		if lr.Errno != 0 {
			return out, fmt.Errorf("百度列表被拒 errno=%d（-9=提取码未验证, -6=cookie失效, 2=参数错误）", lr.Errno)
		}
		if len(lr.List) == 0 {
			break
		}
		for _, f := range lr.List {
			fsid := f.FSID.String()
			if fsid == "" || fsid == "0" {
				continue
			}
			size, _ := strconv.ParseInt(f.Size.String(), 10, 64)
			out = append(out, Entry{
				ID: fsid, Name: f.Filename,
				IsDir:  f.IsDir.String() == "1",
				Size:   size,
				Parent: dir,
				Depth:  depth,
				path:   f.Path,
			})
		}
		if len(lr.List) < num {
			break
		}
		if page > 200 {
			return out, fmt.Errorf("百度分页超过 200 页，疑似接口异常")
		}
	}
	return out, nil
}

// walk 递归列举子目录
func (b *BaiduTransferor) walk(ctx context.Context, sid, ukv, surl string, parents []Entry, depth int) ([]Entry, error) {
	if depth > b.common.MaxDepth {
		return nil, nil
	}
	var out []Entry
	for _, p := range parents {
		if !p.IsDir || p.path == "" {
			continue
		}
		kids, err := b.listDir(ctx, sid, ukv, surl, p.path, depth)
		if err != nil {
			return out, err
		}
		out = append(out, kids...)
		deeper, err := b.walk(ctx, sid, ukv, surl, kids, depth+1)
		if err != nil {
			return out, err
		}
		out = append(out, deeper...)
	}
	return out, nil
}

func (b *BaiduTransferor) tokenOrEmpty() string {
	return b.bdstoken // 拿不到就传空串，比硬写 "null" 更接近浏览器行为
}

// ensureBDCLND 在 Jar 里补写 BDCLND（部分情况下 Set-Cookie 不落 Jar）
func (b *BaiduTransferor) ensureBDCLND(randsk string) {
	if b.HTTP.Jar == nil {
		return
	}
	u, err := url.Parse(b.base() + "/")
	if err != nil {
		return
	}
	for _, c := range b.HTTP.Jar.Cookies(u) {
		if c.Name == "BDCLND" && c.Value != "" {
			return // Jar 已有，无需补
		}
	}
	b.HTTP.Jar.SetCookies(u, []*http.Cookie{{
		Name: "BDCLND", Value: randsk, Path: "/",
	}})
}

func firstGroup(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}
