package transfer

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"fnos-enhance/internal/linker"
)

// QuarkTransferor 夸克转存器
type QuarkTransferor struct {
	cfg    QuarkConfig
	common Config
	HTTP   *http.Client

	// baseURL 仅供测试注入，空则用真实域名
	baseURL string
}

func (q *QuarkTransferor) base() string {
	if q.baseURL != "" {
		return q.baseURL
	}
	return "https://drive-h.quark.cn"
}

// 夸克单页上限 50，传更大会被忽略
const quarkMaxPageSize = 50

type quarkItem struct {
	FID           string `json:"fid"`
	FileName      string `json:"file_name"`
	ShareFidToken string `json:"share_fid_token"`
	Dir           bool   `json:"dir"`
	FileType      int    `json:"file_type"`
	Size          int64  `json:"size"`
}

func (i quarkItem) isDir() bool {
	// dir 字段优先；部分响应只给 file_type（0=目录 1=文件）
	return i.Dir || i.FileType == 0
}

type quarkDetailResp struct {
	Status  int    `json:"status"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		List []quarkItem `json:"list"`
	} `json:"data"`
	Metadata struct {
		Total int `json:"_total"`
		Size  int `json:"_size"`
		Page  int `json:"_page"`
	} `json:"metadata"`
}

type quarkTokenResp struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
	Data    struct {
		Stoken string `json:"stoken"`
	} `json:"data"`
}

type quarkSaveResp struct {
	Status  int    `json:"status"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		TaskID string `json:"task_id"`
	} `json:"data"`
}

func (q *QuarkTransferor) headers(req *http.Request) {
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Referer", "https://pan.quark.cn/")
	req.Header.Set("Cookie", q.cfg.Cookie)
}

// Transfer 夸克分享转存：token → detail(分页+递归) → save
func (q *QuarkTransferor) Transfer(ctx context.Context, link linker.ShareLink, dryRun bool) (*TransferResult, error) {
	if link.ID == "" {
		return nil, fmt.Errorf("无法解析夸克链接: %s", link.Link)
	}
	// 注意：不在这里查 cookie。实测 `share/sharepage/token` 与 `detail`
	// 无 cookie 也能读公开分享，所以 dry-run 预览应当免凭据。
	// cookie 只在真正 save 前校验（见下方）。
	if !dryRun && q.cfg.Cookie == "" {
		return nil, fmt.Errorf("夸克 cookie 未配置：列举无需凭据，但转存必须登录（设置 QUARK_COOKIE）")
	}
	pwdID := link.ID

	// 1. 换取 stoken（提取码走 passcode）
	var tr quarkTokenResp
	err := doJSON(ctx, q.HTTP, q.common.MaxRetries, http.MethodPost,
		q.base()+"/1/clouddrive/share/sharepage/token?pr=ucpro&fr=pc&uc_param_str=",
		map[string]interface{}{
			"pwd_id":                            pwdID,
			"passcode":                          link.Pwd,
			"support_visit_limit_private_share": true,
		}, q.headers, &tr)
	if err != nil {
		return nil, fmt.Errorf("夸克取 stoken 失败: %w", err)
	}
	if tr.Status != 200 || tr.Data.Stoken == "" {
		return nil, fmt.Errorf("夸克分享不可访问（status=%d msg=%s），可能已失效或需提取码", tr.Status, tr.Message)
	}
	stoken := tr.Data.Stoken

	// 2. 列举顶层 + 递归子目录（审计 P0-4：旧代码只取第一页 50 条且不递归）
	top, err := q.listDir(ctx, pwdID, stoken, "0", 0)
	if err != nil {
		return nil, err
	}
	if len(top) == 0 {
		return nil, fmt.Errorf("分享无文件")
	}

	all := append([]Entry(nil), top...)
	if q.common.MaxDepth > 0 {
		sub, err := q.walk(ctx, pwdID, stoken, top, 1)
		if err != nil {
			// 递归失败不阻断转存：顶层 ID 已足够提交（网盘会保留目录内部结构）
			fmt.Printf("警告: 夸克递归列举中断（%v），仅按顶层条目转存\n", err)
		} else {
			all = append(all, sub...)
		}
	}

	names := make([]string, 0, len(top))
	fidList := make([]string, 0, len(top))
	tokenList := make([]string, 0, len(top))
	for _, e := range top {
		names = append(names, e.Name)
		fidList = append(fidList, e.ID)
		if e.shareFidToken != "" {
			tokenList = append(tokenList, e.shareFidToken)
		}
	}

	result := &TransferResult{
		Provider: "夸克", Link: link.Link,
		Names: names, Entries: all, DryRun: dryRun,
	}
	if dryRun {
		return result, nil
	}

	// 3. 转存（顶层 ID 即可，目录内部结构由网盘侧保留）
	toDir := q.cfg.ToDirFID
	if toDir == "" {
		toDir = "0"
	}
	payload := map[string]interface{}{
		"pwd_id":      pwdID,
		"stoken":      stoken,
		"pdir_fid":    "0",
		"to_pdir_fid": toDir,
		"scene":       "link",
		"fid_list":    fidList,
	}
	// share_fid_token 与 fid_list 必须等长，缺失则不传（部分分享无此字段）
	if len(tokenList) == len(fidList) {
		payload["fid_token_list"] = tokenList
	}

	var sr quarkSaveResp
	err = doJSON(ctx, q.HTTP, q.common.MaxRetries, http.MethodPost,
		q.base()+"/1/clouddrive/share/sharepage/save?pr=ucpro&fr=pc&uc_param_str=",
		payload, q.headers, &sr)
	if err != nil {
		return nil, fmt.Errorf("夸克转存请求失败: %w", err)
	}
	if sr.Status != 200 {
		return nil, fmt.Errorf("夸克转存被拒: status=%d code=%d msg=%s", sr.Status, sr.Code, sr.Message)
	}

	result.Transferred = len(fidList)
	return result, nil
}

// listDir 列举一个目录下的全部条目（自动翻页到取完）
func (q *QuarkTransferor) listDir(ctx context.Context, pwdID, stoken, pdirFID string, depth int) ([]Entry, error) {
	size := q.common.PageSize
	if size > quarkMaxPageSize || size <= 0 {
		size = quarkMaxPageSize
	}

	var out []Entry
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		u := fmt.Sprintf(q.base()+"/1/clouddrive/share/sharepage/detail"+
			"?pr=ucpro&fr=pc&uc_param_str=&ver=2&pwd_id=%s&stoken=%s&pdir_fid=%s"+
			"&force=0&_page=%d&_size=%d&_fetch_banner=0&_fetch_share=0&_fetch_total=1",
			url.QueryEscape(pwdID), url.QueryEscape(stoken), url.QueryEscape(pdirFID), page, size)

		var dr quarkDetailResp
		if err := doJSON(ctx, q.HTTP, q.common.MaxRetries, http.MethodGet, u, nil, q.headers, &dr); err != nil {
			return out, fmt.Errorf("夸克列举目录 %s 第 %d 页失败: %w", pdirFID, page, err)
		}
		if dr.Status != 0 && dr.Status != 200 {
			return out, fmt.Errorf("夸克列举目录被拒: status=%d msg=%s", dr.Status, dr.Message)
		}
		if len(dr.Data.List) == 0 {
			break
		}
		for _, it := range dr.Data.List {
			if it.FID == "" {
				continue
			}
			parent := pdirFID
			if parent == "0" {
				parent = ""
			}
			out = append(out, Entry{
				ID: it.FID, Name: it.FileName, IsDir: it.isDir(),
				Size: it.Size, Parent: parent, Depth: depth,
				shareFidToken: it.ShareFidToken,
			})
		}
		// 拿到总数就按总数收敛，否则按"不足一页"收敛
		total := dr.Metadata.Total
		if total > 0 && len(out) >= total {
			break
		}
		if len(dr.Data.List) < size {
			break
		}
		if page > 200 { // 兜底防御无限翻页
			return out, fmt.Errorf("夸克分页超过 200 页，疑似接口异常")
		}
	}
	return out, nil
}

// walk 递归列举子目录，depth 从 1 开始
func (q *QuarkTransferor) walk(ctx context.Context, pwdID, stoken string, parents []Entry, depth int) ([]Entry, error) {
	if depth > q.common.MaxDepth {
		return nil, nil
	}
	var out []Entry
	for _, p := range parents {
		if !p.IsDir {
			continue
		}
		kids, err := q.listDir(ctx, pwdID, stoken, p.ID, depth)
		if err != nil {
			return out, err
		}
		out = append(out, kids...)
		deeper, err := q.walk(ctx, pwdID, stoken, kids, depth+1)
		if err != nil {
			return out, err
		}
		out = append(out, deeper...)
	}
	return out, nil
}
