package linker

import (
	"regexp"
)

// LinkType 链接类型
type LinkType string

const (
	LinkQuark   LinkType = "quark"
	LinkBaidu   LinkType = "baidu"
	LinkGuangYa LinkType = "guangya"
	LinkUnknown LinkType = "unknown"
)

// ShareLink 识别后的分享链接
type ShareLink struct {
	Type    LinkType
	Link    string // 原始链接
	ID      string // 分享 ID
	Pwd     string // 提取码（百度）
}

// 正则表
var linkPatterns = []struct {
	re   *regexp.Regexp
	Type LinkType
}{
	// 夸克：pan.quark.cn/s/xxxxx 或 quark.cn/s/xxxxx
	{regexp.MustCompile(`(?:pan\.)?quark\.cn/s/([A-Za-z0-9]+)`), LinkQuark},
	// 百度：pan.baidu.com/s/xxxxx?pwd=yyyy
	{regexp.MustCompile(`pan\.baidu\.com/s/([A-Za-z0-9_-]+)(?:\?pwd=([A-Za-z0-9]{4}))?`), LinkBaidu},
	// 光鸭：guangyapan.com/s/xxxxx
	{regexp.MustCompile(`guangyapan\.com/s/([A-Za-z0-9_]+)`), LinkGuangYa},
}

// ParseLinks 从文本中提取所有分享链接
func ParseLinks(text string) []ShareLink {
	var results []ShareLink
	for _, p := range linkPatterns {
		matches := p.re.FindAllStringSubmatch(text, -1)
		for _, m := range matches {
			sl := ShareLink{
				Type: p.Type,
				Link: m[0],
				ID:   m[1],
			}
			if len(m) > 2 && m[2] != "" {
				sl.Pwd = m[2]
			}
			results = append(results, sl)
		}
	}
	return results
}

// ParseLink 解析单个链接
func ParseLink(link string) ShareLink {
	for _, p := range linkPatterns {
		if m := p.re.FindStringSubmatch(link); m != nil {
			sl := ShareLink{
				Type: p.Type,
				Link: m[0],
				ID:   m[1],
			}
			if len(m) > 2 && m[2] != "" {
				sl.Pwd = m[2]
			}
			return sl
		}
	}
	return ShareLink{Type: LinkUnknown, Link: link}
}
