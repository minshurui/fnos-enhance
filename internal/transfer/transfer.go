package transfer

import (
	"fmt"
	"net/http"
	"time"

	"fnos-enhance/internal/linker"
)

// TransferResult 转存结果
type TransferResult struct {
	Names    []string // 转存后的文件名列表
	Provider string   // 夸克/百度/光鸭
	Link     string   // 原始链接
}

// Transferor 转存接口
type Transferor interface {
	Transfer(link linker.ShareLink) (*TransferResult, error)
}

// NewTransferor 根据链接类型创建转存器
// quarkCookie: 夸克 cookie (从 CloudDrive2 获取)
// baiduCookie: 百度 cookie (BDUSS 等)
// guangyaClientID/Secret: 光鸭开发者凭据
func NewTransferor(quarkCookie, baiduCookie, guangyaClientID, guangyaClientSecret string) Transferor {
	return &multiTransferor{
		quark:   &QuarkTransferor{Cookie: quarkCookie, HTTP: httpClient()},
		baidu:   &BaiduTransferor{Cookie: baiduCookie, HTTP: httpClient()},
		guangya: &GuangYaTransferor{ClientID: guangyaClientID, ClientSecret: guangyaClientSecret, HTTP: httpClient()},
	}
}

type multiTransferor struct {
	quark   *QuarkTransferor
	baidu   *BaiduTransferor
	guangya *GuangYaTransferor
}

func (m *multiTransferor) Transfer(link linker.ShareLink) (*TransferResult, error) {
	switch link.Type {
	case linker.LinkQuark:
		return m.quark.Transfer(link)
	case linker.LinkBaidu:
		return m.baidu.Transfer(link)
	case linker.LinkGuangYa:
		return m.guangya.Transfer(link)
	default:
		return nil, fmt.Errorf("不支持的链接类型: %s", link.Link)
	}
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}
