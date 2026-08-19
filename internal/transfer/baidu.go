package transfer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"fnos-enhance/internal/linker"
)

// BaiduTransferor 百度转存器
type BaiduTransferor struct {
	Cookie string
	HTTP   *http.Client
}

func (b *BaiduTransferor) Transfer(link linker.ShareLink) (*TransferResult, error) {
	if link.ID == "" {
		return nil, fmt.Errorf("无法解析百度链接")
	}
	if b.Cookie == "" {
		return nil, fmt.Errorf("百度 cookie 未配置")
	}

	surl := link.ID
	pwd := link.Pwd

	// 1. 访问分享页获取 shareid/uk
	suffix := ""
	if pwd != "" {
		suffix = "?pwd=" + pwd
	}
	req, _ := http.NewRequest("GET", "https://pan.baidu.com/s/"+surl+suffix, nil)
	req.Header.Set("Referer", "https://pan.baidu.com/disk/home")
	body, _, err := b.do(req)
	if err != nil {
		return nil, err
	}
	html := string(body)
	shareID := regexp.MustCompile(`"shareid":\s*(\d+)`).FindStringSubmatch(html)
	uk := regexp.MustCompile(`"uk":\s*(\d+)`).FindStringSubmatch(html)
	if shareID == nil || uk == nil {
		return nil, fmt.Errorf("链接无效或需验证（页面无 shareid）")
	}
	sid, ukv := shareID[1], uk[1]

	// 2. 验证提取码
	if pwd != "" {
		vreq, _ := http.NewRequest("POST",
			fmt.Sprintf("https://pan.baidu.com/share/verify?shareid=%s&time=%d&clienttype=1&uk=%s", sid, time.Now().UnixMilli(), ukv),
			strings.NewReader("pwd="+pwd+"&vcode=null&vcode_str=null&bdstoken=null"))
		vreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		vreq.Header.Set("Referer", "https://pan.baidu.com/s/"+surl)
		buf, _, err := b.do(vreq)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(string(buf), `"errno":0`) {
			return nil, fmt.Errorf("提取码验证失败")
		}
		// 重新访问获取 BDCLND cookie
		req2, _ := http.NewRequest("GET", "https://pan.baidu.com/s/"+surl, nil)
		b.do(req2)
	}

	// 3. 获取文件列表
	lreq, _ := http.NewRequest("GET",
		fmt.Sprintf("https://pan.baidu.com/share/list?shareid=%s&uk=%s&app_id=250528&web=5&shorturl=%s&root=1&num=100",
			sid, ukv, surl[1:]), nil)
	lreq.Header.Set("Referer", "https://pan.baidu.com/share/init?surl="+surl)
	lbuf, _, err := b.do(lreq)
	if err != nil {
		return nil, err
	}
	var lr struct {
		Errno int `json:"errno"`
		List  []struct {
			FSID   int64  `json:"fs_id"`
			Server string `json:"server_filename"`
		} `json:"list"`
	}
	if err := json.Unmarshal(lbuf, &lr); err != nil || lr.Errno != 0 || len(lr.List) == 0 {
		return nil, fmt.Errorf("分享列表失败")
	}
	var fsIDs []string
	var names []string
	for _, f := range lr.List {
		fsIDs = append(fsIDs, fmt.Sprintf("%d", f.FSID))
		names = append(names, f.Server)
	}

	// 4. 转存到根目录
	treq, _ := http.NewRequest("POST",
		fmt.Sprintf("https://pan.baidu.com/share/transfer?app_id=250528&channel=chunlei&clienttype=0&web=1&shareid=%s&from=%s", sid, ukv),
		strings.NewReader("fsidlist="+strings.Join(fsIDs, ",")+"&path=/"))
	treq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	treq.Header.Set("Referer", "https://pan.baidu.com/share/init?surl="+surl)
	tbuf, _, err := b.do(treq)
	if err != nil {
		return nil, err
	}
	var tr struct {
		Errno int `json:"errno"`
	}
	json.Unmarshal(tbuf, &tr)
	if tr.Errno != 0 && tr.Errno != 4 { // 4=已存在
		return nil, fmt.Errorf("转存失败 errno=%d", tr.Errno)
	}

	return &TransferResult{Names: names, Provider: "百度", Link: link.Link}, nil
}

func (b *BaiduTransferor) do(req *http.Request) ([]byte, http.Header, error) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	if b.Cookie != "" {
		req.Header.Set("Cookie", b.Cookie)
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	return buf, resp.Header, nil
}
