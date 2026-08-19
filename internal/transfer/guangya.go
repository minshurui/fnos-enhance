package transfer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"fnos-enhance/internal/linker"
)

// GuangYaTransferor 光鸭转存器
type GuangYaTransferor struct {
	ClientID     string
	ClientSecret string
	AccessToken  string // 预置的 access_token（可选，优先使用）
	HTTP         *http.Client
}

func (g *GuangYaTransferor) Transfer(link linker.ShareLink) (*TransferResult, error) {
	if link.ID == "" {
		return nil, fmt.Errorf("无法解析光鸭链接")
	}

	shareID := link.ID
	token := g.AccessToken
	if token == "" {
		return nil, fmt.Errorf("光鸭 access_token 未配置")
	}

	// 1. 获取分享访问 token
	r, err := g.api("/userres/v1/get_share_access_token", map[string]interface{}{"shareId": shareID}, "")
	if err != nil {
		return nil, err
	}
	var sat string
	if rd, ok := r["data"].(map[string]interface{}); ok {
		sat, _ = rd["accessToken"].(string)
	}
	if sat == "" {
		return nil, fmt.Errorf("分享访问失败")
	}

	// 2. 获取文件列表
	f, err := g.api("/userres/v1/get_share_page_files_list",
		map[string]interface{}{"pageSize": 100, "accessToken": sat, "orderBy": 0, "sortType": 0, "parentId": ""}, "")
	if err != nil {
		return nil, err
	}
	var flist []interface{}
	if fd, ok := f["data"].(map[string]interface{}); ok {
		flist, _ = fd["fileList"].([]interface{})
		if len(flist) == 0 {
			flist, _ = fd["list"].([]interface{})
		}
	}
	var fileIDs []string
	var names []string
	for _, it := range flist {
		item, _ := it.(map[string]interface{})
		fid, _ := item["fileId"].(string)
		name, _ := item["fileName"].(string)
		if fid == "" {
			fid, _ = item["id"].(string)
		}
		if fid != "" {
			fileIDs = append(fileIDs, fid)
			names = append(names, name)
		}
	}
	if len(fileIDs) == 0 {
		return nil, fmt.Errorf("分享无文件")
	}

	// 3. 转存到云盘
	s, err := g.api("/userres/v1/restore_share",
		map[string]interface{}{"accessToken": sat, "fileIds": fileIDs, "parentId": ""}, token)
	if err != nil {
		return nil, err
	}
	if msg, _ := s["msg"].(string); msg != "success" {
		return nil, fmt.Errorf("转存失败: %v", s["msg"])
	}

	return &TransferResult{Names: names, Provider: "光鸭", Link: link.Link}, nil
}

func (g *GuangYaTransferor) api(path string, data map[string]interface{}, token string) (map[string]interface{}, error) {
	body, _ := json.Marshal(data)
	req, _ := http.NewRequest("POST", "https://api.guangyapan.com"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	var r map[string]interface{}
	if err := json.Unmarshal(buf, &r); err != nil {
		return nil, fmt.Errorf("光鸭响应解析失败")
	}
	return r, nil
}

var guangyaLinkRe = regexp.MustCompile(`guangyapan\.com/s/([A-Za-z0-9_]+)`)

func ExtractGuangYaID(link string) string {
	m := guangyaLinkRe.FindStringSubmatch(link)
	if m == nil {
		return ""
	}
	return m[1]
}
