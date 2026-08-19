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
		input  string
		LinkType  LinkType
		id     string
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
