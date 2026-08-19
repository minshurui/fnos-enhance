# 飞牛增强层 (FnOS Enhancement Layer)

TG 链接 → 自动转存 → 智能改名 → 飞牛影视可用

## 架构

```
用户发链接 → linker(识别) → transfer(转存) → renamer(改名) → CloudDrive2目录 → 飞牛影视扫描
```

## 模块

| 模块 | 功能 |
|---|---|
| `internal/linker` | 链接识别（夸克/百度/光鸭） |
| `internal/transfer` | 网盘转存（对接三个云盘 API） |
| `internal/renamer` | 改名引擎（去广告 → TMDB → 规范路径） |
| `cmd/fnosctl` | CLI 工具 |

## 用法

```bash
# 解析链接
./fnosctl parse "帮我转存 https://pan.quark.cn/s/abc123"

# 测试改名
./fnosctl rename "[www.66影视.com]吞噬星空.第81集.HD1080P.mp4"

# 交互模式
./fnosctl run config.json
```

## 改名规则

**电影**: `电影/片名 (年份)/片名 (年份).ext`
**电视剧**: `电视剧/片名 (年份)/Season XX/片名 - SXXEXX.ext`

## 配置

```json
{
  "quark_cookie": "你的夸克 cookie",
  "baidu_cookie": "你的百度 cookie (BDUSS...)",
  "guangya_token": "光鸭 access_token",
  "tmdb_api_key": "<从环境变量 TMDB_API_KEY 读取，勿写入文件>",
  "clouddrive_mount": "/vol2/1000/CloudDrive/影视"
}
```

## 进度

- [x] M1: PRD
- [x] M2: 改名引擎
- [x] M3: 链接识别 + 转存
- [ ] M4: TG Bot 集成
- [ ] M5: 飞牛对接
- [ ] M6: 搜索/追剧增强
