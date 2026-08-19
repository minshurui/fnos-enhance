package transfer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"fnos-enhance/internal/linker"
)

// QuarkTransferor 夸克转存器
type QuarkTransferor struct {
	Cookie string
	HTTP   *http.Client
}

func (q *QuarkTransferor) Transfer(link linker.ShareLink) (*TransferResult, error) {
	if link.ID == "" {
		return nil, fmt.Errorf("无法解析夸克链接")
	}
	if q.Cookie == "" {
		return nil, fmt.Errorf("夸克 cookie 未配置")
	}

	pwdID := link.ID

	// 1. 获取 stoken
	r, err := q.api("/1/clouddrive/share/sharepage/token?pr=ucpro&fr=pc&uc_param_str=",
		map[string]interface{}{"pwd_id": pwdID, "passcode": "", "support_visit_limit_private_share": true})
	if err != nil {
		return nil, err
	}
	if st, ok := r["status"].(float64); ok && st != 200 {
		return nil, fmt.Errorf("夸克分享不存在: %v", r["message"])
	}
	stoken := ""
	if data, ok := r["data"].(map[string]interface{}); ok {
		stoken, _ = data["stoken"].(string)
	}
	if stoken == "" {
		return nil, fmt.Errorf("拿不到 stoken")
	}

	// 2. 获取文件列表
	d, err := q.api("/1/clouddrive/share/sharepage/detail?pr=ucpro&fr=pc&uc_param_str=&ver=2&pwd_id="+pwdID+
		"&stoken="+url.QueryEscape(stoken)+"&pdir_fid=0&force=0&_page=1&_size=50&_fetch_banner=1&_fetch_share=1&_fetch_total=1",
		nil)
	if err != nil {
		return nil, err
	}
	var list []interface{}
	if dd, ok := d["data"].(map[string]interface{}); ok {
		list, _ = dd["list"].([]interface{})
	}
	var fileIDs []string
	var names []string
	for _, it := range list {
		f, _ := it.(map[string]interface{})
		fid, _ := f["fid"].(string)
		name, _ := f["file_name"].(string)
		if fid != "" {
			fileIDs = append(fileIDs, fid)
			names = append(names, name)
		}
	}
	if len(fileIDs) == 0 {
		return nil, fmt.Errorf("分享无文件")
	}

	// 3. 转存
	_, err = q.api("/1/clouddrive/share/sharepage/save?pr=ucpro&fr=pc&uc_param_str=",
		map[string]interface{}{
			"pwd_id": pwdID, "pdir_fid": "0", "to_pdir_fid": "0",
			"scene": "link", "fid_list": fileIDs, "stoken": stoken,
		})
	if err != nil {
		return nil, err
	}

	return &TransferResult{Names: names, Provider: "夸克", Link: link.Link}, nil
}

func (q *QuarkTransferor) api(path string, data map[string]interface{}) (map[string]interface{}, error) {
	var body io.Reader
	if data != nil {
		b, _ := json.Marshal(data)
		body = bytes.NewReader(b)
	}
	method := "GET"
	if data != nil {
		method = "POST"
	}
	req, _ := http.NewRequest(method, "https://drive-h.quark.cn"+path, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/126.0")
	req.Header.Set("Referer", "https://pan.quark.cn/")
	req.Header.Set("Cookie", q.Cookie)
	resp, err := q.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	var r map[string]interface{}
	if err := json.Unmarshal(buf, &r); err != nil {
		return nil, fmt.Errorf("夸克响应解析失败: %s", string(buf[:min(len(buf), 150)]))
	}
	return r, nil
}
