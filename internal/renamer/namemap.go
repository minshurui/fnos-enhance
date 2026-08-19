package renamer

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
)

// NameMap 乱码名/谐音名 → 规范片名 映射
// 修 P1-4：现有 tg-media-bot 有 quark-name-map.json，新项目缺失属功能回归
// 兼容格式：{"H 椛幵婂秀": "花开锦秀", "TCNY": "天才女友", "_说明": "..."}
type NameMap struct {
	mu    sync.RWMutex
	exact map[string]string // 归一化键 → 规范片名
}

var reNormKey = regexp.MustCompile(`[\s\.\-_·‧•、，,:：;；!！?？「」“”‘’（()）\[\]【】]+`)

// normKey 归一化：去空白/点/连字符，转小写，便于容错匹配
func normKey(s string) string {
	s = reNormKey.ReplaceAllString(s, "")
	return strings.ToLower(strings.TrimSpace(s))
}

func NewNameMap() *NameMap {
	return &NameMap{exact: make(map[string]string)}
}

// LoadNameMap 从 quark-name-map.json 加载
func LoadNameMap(path string) (*NameMap, error) {
	nm := NewNameMap()
	data, err := os.ReadFile(path)
	if err != nil {
		return nm, err // 返回空映射，不阻断主流程
	}
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nm, err
	}
	for k, v := range raw {
		if strings.HasPrefix(k, "_") { // 跳过 _说明 等元数据键
			continue
		}
		nm.Set(k, v)
	}
	return nm, nil
}

func (n *NameMap) Set(alias, canonical string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.exact[normKey(alias)] = canonical
}

// Resolve 解析片名；返回规范名与是否命中
func (n *NameMap) Resolve(title string) (string, bool) {
	if title == "" {
		return title, false
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	if v, ok := n.exact[normKey(title)]; ok {
		return v, true
	}
	return title, false
}

// Len 映射条数
func (n *NameMap) Len() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.exact)
}

// Apply 就地应用映射到 MediaInfo.Title
func (n *NameMap) Apply(info *MediaInfo) bool {
	if resolved, ok := n.Resolve(info.Title); ok {
		info.Title = resolved
		return true
	}
	return false
}
