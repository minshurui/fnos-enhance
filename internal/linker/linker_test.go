package linker

import (
	"testing"
)

func TestParseLinks(t *testing.T) {
	tests := []struct {
		input    string
		count    int
		first    LinkType
		firstID  string
		firstPwd string
	}{
		// 单链接
		{"帮我转存 https://pan.quark.cn/s/abc123def", 1, LinkQuark, "abc123def", ""},
		{"https://pan.baidu.com/s/1abcDEF?pwd=abcd", 1, LinkBaidu, "1abcDEF", "abcd"},
		{"https://www.guangyapan.com/s/xyz_789", 1, LinkGuangYa, "xyz_789", ""},
		// 无链接
		{"今天天气不错", 0, LinkUnknown, "", ""},
		// 混合文本
		{"夸克 https://pan.quark.cn/s/aaa111 百度 https://pan.baidu.com/s/bbb222?pwd=1234", 2, LinkQuark, "aaa111", ""},
		// 百度无提取码
		{"https://pan.baidu.com/s/1test", 1, LinkBaidu, "1test", ""},
	}

	for _, tt := range tests {
		links := ParseLinks(tt.input)
		if len(links) != tt.count {
			t.Errorf("ParseLinks(%q) got %d links, want %d", tt.input, len(links), tt.count)
			continue
		}
		if tt.count == 0 {
			continue
		}
		if links[0].Type != tt.first {
			t.Errorf("ParseLinks(%q)[0].Type = %q, want %q", tt.input, links[0].Type, tt.first)
		}
		if links[0].ID != tt.firstID {
			t.Errorf("ParseLinks(%q)[0].ID = %q, want %q", tt.input, links[0].ID, tt.firstID)
		}
		if links[0].Pwd != tt.firstPwd {
			t.Errorf("ParseLinks(%q)[0].Pwd = %q, want %q", tt.input, links[0].Pwd, tt.firstPwd)
		}
	}
}

func TestParseLink(t *testing.T) {
	tests := []struct {
		input    string
		LinkType LinkType
		id       string
	}{
		{"https://pan.quark.cn/s/abc123", LinkQuark, "abc123"},
		{"https://pan.baidu.com/s/1test?pwd=abcd", LinkBaidu, "1test"},
		{"https://guangyapan.com/s/xyz", LinkGuangYa, "xyz"},
		{"https://example.com/unknown", LinkUnknown, ""},
	}

	for _, tt := range tests {
		sl := ParseLink(tt.input)
		if sl.Type != tt.LinkType {
			t.Errorf("ParseLink(%q).Type = %q, want %q", tt.input, sl.Type, tt.LinkType)
		}
		if sl.ID != tt.id {
			t.Errorf("ParseLink(%q).ID = %q, want %q", tt.input, sl.ID, tt.id)
		}
	}
}

// ------------------------------------------------------------
// 审计 P1-8 回归：域名边界（防误转存 / SSRF 面）
// ------------------------------------------------------------

func TestDomainBoundary_RejectsLookalikes(t *testing.T) {
	// 这些都 **不能** 被识别为任何网盘链接
	evil := []string{
		"https://notquark.cn/s/evil123",         // 后缀混淆：包含 quark.cn 子串
		"https://evilquark.cn/s/evil123",        // 同上
		"https://myquark.cn/s/abc",              // 同上
		"https://quark.cn.evil.com/s/abc",       // 后缀挂载
		"https://pan.baidu.com.evil.com/s/abc",  // 后缀挂载
		"https://notpan.baidu.com/s/abc",        // 前缀混淆
		"https://evil.com/pan.quark.cn/s/abc",   // 路径里塞域名
		"https://fakeguangyapan.com/s/abc",      // 后缀混淆
		"https://guangyapan.com.evil.net/s/abc", // 后缀挂载
	}
	for _, u := range evil {
		if got := ParseLink(u); got.Type != LinkUnknown {
			t.Errorf("仿冒域名被误判: %q -> Type=%s ID=%s（会把攻击者的 ID 送进转存器）",
				u, got.Type, got.ID)
		}
		if links := ParseLinks("看这个 " + u + " 谢谢"); len(links) != 0 {
			t.Errorf("仿冒域名在文本中被误提取: %q -> %+v", u, links)
		}
	}
}

func TestDomainBoundary_AcceptsRealDomains(t *testing.T) {
	// 真实域名及其合法子域必须仍然识别
	good := []struct {
		url  string
		want LinkType
		id   string
	}{
		{"https://pan.quark.cn/s/abc123", LinkQuark, "abc123"},
		{"https://quark.cn/s/abc123", LinkQuark, "abc123"},
		{"http://pan.quark.cn/s/abc123", LinkQuark, "abc123"},
		{"pan.quark.cn/s/abc123", LinkQuark, "abc123"}, // 无协议
		{"https://pan.baidu.com/s/1abcDEF?pwd=abcd", LinkBaidu, "1abcDEF"},
		{"https://guangyapan.com/s/xyz_789", LinkGuangYa, "xyz_789"},
		{"https://www.guangyapan.com/s/xyz_789", LinkGuangYa, "xyz_789"},
	}
	for _, g := range good {
		got := ParseLink(g.url)
		if got.Type != g.want || got.ID != g.id {
			t.Errorf("真实链接识别失败: %q -> Type=%s ID=%s，期望 Type=%s ID=%s",
				g.url, got.Type, got.ID, g.want, g.id)
		}
	}
}

func TestDomainBoundary_LinkFieldIsClean(t *testing.T) {
	// 前导边界字符不能混进 Link 字段
	links := ParseLinks("看这个:https://pan.quark.cn/s/abc123 完")
	if len(links) != 1 {
		t.Fatalf("期望 1 条，得到 %d", len(links))
	}
	if links[0].Link != "pan.quark.cn/s/abc123" {
		t.Errorf("Link 字段不干净: %q", links[0].Link)
	}
}

// ------------------------------------------------------------
// 审计 P1-5 回归：同一链接去重（否则会被转存两次）
// ------------------------------------------------------------

func TestParseLinks_Dedupes(t *testing.T) {
	text := `第一次 https://pan.quark.cn/s/aaa111
	重复贴 https://pan.quark.cn/s/aaa111
	换个写法 pan.quark.cn/s/aaa111`
	links := ParseLinks(text)
	if len(links) != 1 {
		t.Errorf("同一分享应只返回 1 条，得到 %d 条（旧代码会转存 3 次）: %+v", len(links), links)
	}
}

func TestParseLinks_DifferentIDsNotDeduped(t *testing.T) {
	text := "https://pan.quark.cn/s/aaa111 https://pan.quark.cn/s/bbb222"
	links := ParseLinks(text)
	if len(links) != 2 {
		t.Errorf("不同分享不应被去重，期望 2，得到 %d", len(links))
	}
}

func TestParseLinks_OrderedByAppearance(t *testing.T) {
	// 百度在前、夸克在后，返回顺序必须与文本一致
	text := "先百度 https://pan.baidu.com/s/1zzz?pwd=1234 再夸克 https://pan.quark.cn/s/aaa111"
	links := ParseLinks(text)
	if len(links) != 2 {
		t.Fatalf("期望 2 条，得到 %d", len(links))
	}
	if links[0].Type != LinkBaidu || links[1].Type != LinkQuark {
		t.Errorf("顺序错误: [0]=%s [1]=%s，期望 baidu, quark", links[0].Type, links[1].Type)
	}
}

func TestParseLinks_AdjacentLinks(t *testing.T) {
	// 边界字符被吃掉后不能漏掉相邻链接
	text := "a=pan.quark.cn/s/aaa111&b=pan.quark.cn/s/bbb222"
	links := ParseLinks(text)
	if len(links) != 2 {
		t.Errorf("相邻链接漏识别: 期望 2，得到 %d: %+v", len(links), links)
	}
}
