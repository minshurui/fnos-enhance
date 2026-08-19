package linker

import (
	"regexp"
	"sort"
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
	Type LinkType
	Link string // 规范化后的链接（域名+路径，不含前导边界字符）
	ID   string // 分享 ID
	Pwd  string // 提取码
}

// 正则表
//
// 审计 P1-8 修复：旧正则 `(?:pan\.)?quark\.cn/s/(...)` 会把
// `notquark.cn/s/evil123` 判成夸克链接（`quark.cn` 子串命中），
// 导致把攻击者域名的 ID 送进夸克转存器（误转存 / SSRF 面）。
//
// 现在的防御有三层：
//  1. 前导边界 `(?:^|[^A-Za-z0-9._-])`：域名左侧必须是行首或非域名字符，
//     杜绝 `notquark.cn` / `evilquark.cn` 这类后缀混淆
//  2. 子域名显式化 `(?:[A-Za-z0-9-]+\.)*`：只允许真子域，不允许拼接
//  3. `/s/` 紧跟域名：杜绝 `pan.baidu.com.evil.com/s/x` 这类后缀挂载
//
// Go 的 RE2 不支持后向断言，所以边界字符被吃进匹配里，
// 因此用 group1 捕获"干净链接"，不能再用 m[0]。
//
// 边界集合的关键细节：**排除单个 `/`**，否则
// `https://evil.com/pan.quark.cn/s/abc`（域名出现在别人路径里）
// 会被误当成主机。合法链接的 `//`（协议分隔符）单独放行。
var linkPatterns = []struct {
	re     *regexp.Regexp
	Type   LinkType
	pwdIdx int // 提取码所在捕获组下标，0 = 无
}{
	// 夸克：[子域.]quark.cn/s/xxxxx
	{regexp.MustCompile(`(?:^|//|[^A-Za-z0-9._/-])((?:[A-Za-z0-9-]+\.)*quark\.cn/s/([A-Za-z0-9]{1,128}))`),
		LinkQuark, 0},
	// 百度：[子域.]pan.baidu.com/s/xxxxx[?pwd=yyyy]
	{regexp.MustCompile(`(?:^|//|[^A-Za-z0-9._/-])((?:[A-Za-z0-9-]+\.)*pan\.baidu\.com/s/([A-Za-z0-9_-]{1,128})(?:\?pwd=([A-Za-z0-9]{4}))?)`),
		LinkBaidu, 3},
	// 光鸭：[子域.]guangyapan.com/s/xxxxx
	{regexp.MustCompile(`(?:^|//|[^A-Za-z0-9._/-])((?:[A-Za-z0-9-]+\.)*guangyapan\.com/s/([A-Za-z0-9_-]{1,128}))`),
		LinkGuangYa, 0},
}

// ParseLinks 从文本中提取所有分享链接
//
// 保证：
//   - 按在文本中出现的先后顺序返回
//   - 同一 (类型, ID) 只返回一次（审计 P1-5：同链接出现两次会被转存两次）
func ParseLinks(text string) []ShareLink {
	type hit struct {
		pos  int
		link ShareLink
	}
	var hits []hit

	for _, p := range linkPatterns {
		idxs := p.re.FindAllStringSubmatchIndex(text, -1)
		for _, ix := range idxs {
			// ix[2],ix[3] = group1（干净链接）；ix[4],ix[5] = group2（ID）
			if len(ix) < 6 || ix[2] < 0 || ix[4] < 0 {
				continue
			}
			sl := ShareLink{
				Type: p.Type,
				Link: text[ix[2]:ix[3]],
				ID:   text[ix[4]:ix[5]],
			}
			if p.pwdIdx > 0 {
				lo, hi := 2*p.pwdIdx, 2*p.pwdIdx+1
				if len(ix) > hi && ix[lo] >= 0 {
					sl.Pwd = text[ix[lo]:ix[hi]]
				}
			}
			hits = append(hits, hit{pos: ix[2], link: sl})
		}
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].pos < hits[j].pos })

	seen := make(map[string]bool, len(hits))
	results := make([]ShareLink, 0, len(hits))
	for _, h := range hits {
		key := string(h.link.Type) + "|" + h.link.ID
		if seen[key] {
			continue // 去重：同一分享只处理一次
		}
		seen[key] = true
		results = append(results, h.link)
	}
	return results
}

// ParseLink 解析单个链接；无法识别时返回 LinkUnknown
func ParseLink(link string) ShareLink {
	if ls := ParseLinks(link); len(ls) > 0 {
		return ls[0]
	}
	return ShareLink{Type: LinkUnknown, Link: link}
}
