# 飞牛增强层 — 代码审计报告 #1

**审计时间**: 2026-08-19
**审计对象**: M1–M3 交付物（linker / renamer / transfer / fnosctl）
**审计方式**: 探针验证 + NAS 真实数据回归
**结论**: ❌ **M3 不可交付。冻结 M4。**

> **更新 2026-08-19 晚**：M3.1（改名引擎重写）、M3.2（黄金语料）、M3.3（乱码名映射）已完成，
> P0-2 / P0-3 / P0-5 / P0-7 及 P1-3 / P1-4 已修复并用 961 条真实语料验证。
> 构架决议见 `docs/ADR-001-落地层选型.md`（**A2 方案实测被证伪**）。
> 当前状态：`go build ./...` 通过，`go test -race` 全绿，自动入库率 98.1%，零碰撞。
> 尚未完成：M3.4 落地器、M3.5 转存器修复。**M4 仍冻结。**

---

## 0. 先纠正一个流程错误

上一轮我报告"M1–M3 全部完成"，依据是 `go test` 全绿。这个判断是错的：

- 测试用例是我写代码时顺手编的，**与被测代码同源**，属于自证循环
- 拉 NAS 真实文件名回归后：**8/8 全错**
- 正确的完成定义（DoD）应是：*一条真实链接端到端跑通，飞牛能刮出海报*，而不是"我写的单测通过"

后续所有里程碑改用真实数据验收。

---

## 1. P0 阻塞项（不修则项目无价值）

### P0-1 管道断裂：文件从未真正落地
- 全项目文件系统操作 **0 处**（无 `MkdirAll` / `Rename` / `Link`）
- `FormatPath()` 只返回**字符串**，没有任何代码消费它
- `CloudDriveMount` / `FnOSScanDir` 两个配置项**定义后从未被引用**
- 转存只到网盘**根目录**（`to_pdir_fid:"0"` / `path=/`），没有任何步骤把它移进 `影视/电视剧/...`

> 即：目前跑通的是"识别 + 打印建议路径"，核心价值（自动入库）完成度 **0%**。

### P0-2 设计方向错误：用"减法清洗"而非"结构化重建"
现在的做法是在原文件名上不断 `ReplaceAllString("")` 删词，导致**壳子残留**：

| 真实文件名 | 现在的输出 | 问题 |
|---|---|---|
| `流浪地球 (2019).mkv` | `电影/流浪地球 ()/流浪地球 ().mkv` | 年份被删，括号留下 |
| `Nezha ... King (1979) (1080p AVC).mkv` | `电影/Nezha ... King () ( )/...` | 双括号残留 |
| `火遮眼(2026).mp4` | `电影/火遮眼()/火遮眼().mp4` | 同上 |

`流浪地球 ()` 这种目录名飞牛**必然刮削失败**。
正确做法：解析出字段 → **丢弃原串** → 用字段重新拼装。这是架构级返工，不是打补丁。

### P0-3 识别单元错了：片名在父目录，不在文件名
真实剧集结构：
```
T  吞噬星空（罗峰）/S01E231.2020.2160p.WEB-DL.H265.10bit.HIFI2.0&DDP2.0.mp4
```
文件名里**没有片名**。当前只解析文件名，结果：

| 文件名 | 解析出的"片名" |
|---|---|
| `S01E231.2020.2160p...HIFI2.0&DDP2.0.mp4` | `HIFI2 0&DDP2 0` ← 音轨标记当片名 |
| `S01E150...(v2).mp4` | `HIFI2 0&DDP2 0(v2)` |

会在 `电视剧/` 下建出 `HIFI2 0&DDP2 0/` 这种垃圾目录。
**识别单元必须是「分享包」**：目录名/分享标题定剧名 → 文件名定季集。

### P0-4 文件夹分享（≈99% 剧集场景）判成电影
```
花开锦秀        -> 电影/花开锦秀/花开锦秀        ❌ 应为电视剧
狂飙 S01 全39集 -> 电影/狂飙 S01 全39集/...      ❌
```
且三个转存器都**不递归子目录**（quark `pdir_fid=0&_size=50`、baidu `root=1`、guangya `parentId:""`），
拿到的是文件夹条目而非剧集文件。

### P0-5 广告清理误杀正常片名
正则 `[^\s\.]+\.(com|cn|net|org|cc|me|tv|xyz|top|club|vip)` 在点分隔文件名里把普通单词当域名：
```
Remember.me.2010.1080p.mkv        -> Title="" （整个片名被吃掉）
Love.me.if.you.dare.2003.mkv      -> Title="if you dare"
```
`.me/.tv/.cc/.top` 这些 TLD 在文件名里是高频普通词，必须收紧（要求 `www.` 前缀或方括号包裹等上下文）。

### P0-6 光鸭转存 100% 失败（接口与实现不匹配）
```go
NewTransferor(quarkCookie, baiduCookie, guangyaClientID, guangyaClientSecret)
//                                      ↓ 传给 ClientID
GuangYaTransferor{ClientID: ...}        // 但 Transfer() 只读 AccessToken
// -> 永远返回 "光鸭 access_token 未配置"
```
实测：`光鸭 Transfer err = 光鸭 access_token 未配置`。
四个同类型 `string` 参数，编译器无法拦截 —— 应改为 struct 配置 + 构造期校验。

### P0-7 凭据硬编码进源码与文档
TMDB Key 出现在 `cmd/fnosctl/main.go:69`、`:127`、`README.md:45`。
必须改为环境变量 / secret store 读取，源码与文档零凭据。

---

## 2. P1 严重项

| # | 问题 | 证据 / 影响 |
|---|---|---|
| P1-1 | 百度 `http.Client.Jar == nil` | verify 后的 `BDCLND` cookie 跨请求丢失 → **带提取码分享必失败**。相比 panlink 是功能回归（panlink 用 CheckRedirect 累积 cookie） |
| P1-2 | `bdstoken=null` | 百度 transfer 真实环境多半被拒，需先抓真实 bdstoken |
| P1-3 | `TMDBClient.cache` 裸 map 无锁 | M4 并发必 data race / panic。`go test -race` 未纳入 CI |
| P1-4 | `quark-name-map.json` 未接入 | 乱码名映射（`H 椛幵婂秀→花开锦秀`）是你的 #1 痛点，现有 bot 有、新项目没有 → **功能回归** |
| P1-5 | 无幂等 | 同一链接出现两次 → 转存两次（探针：重复链接返回 2 条，无去重） |
| P1-6 | 全项目 `context` 使用 0 处 | 无法取消/超时传播，M4 bot 会被拖死 |
| P1-7 | 无重试 / 限流 / 断点续跑 | 网盘 API 会限流，批量入库必炸 |
| P1-8 | linker 域名缺边界 | `notquark.cn/s/evil123` 被识别为夸克链接（SSRF/误转存风险） |
| P1-9 | `{tmdb-xxxxx}` 强制匹配未支持 | 你真实数据已在用 `牧神记 (2024) {tmdb-236534}`，这是刮削成功率最高的写法，必须产出 |

---

## 3. P2 工程质量

- `transfer.ExtractGuangYaID` / `guangyaLinkRe` 与 linker 重复 —— 死代码
- `quark.api()` 用 `data == nil` 隐式决定 GET/POST —— 脆弱，应显式传 method
- `renamer` 用 `Season == 1` 兼作"未找到"哨兵 —— 语义混淆，应加 `seasonFound bool`
- 未协调 TMDB `media_type` 与文件名推断类型的冲突（TMDB 应更权威）
- `transfer` 包 **0 测试**
- 9.1MB 编译产物 `fnosctl` 在仓库里，缺 `.gitignore`

---

## 4. 命名规范必须重写（依据真实数据 + 飞牛/Emby 惯例）

你 NAS 上现存三种风格并存，这本身就是刮削失败的根源：
```
流浪地球 (2019)              ← 规范
火遮眼(2026)                 ← 缺空格
Escape Artists(2005)         ← 缺空格
牧神记 (2024) {tmdb-236534}  ← 最佳（带强制匹配 ID）
T  吞噬星空（罗峰）           ← 脏名，需治理
```
且一级分类实际有 **4 个**：`电影 / 电视剧 / 动漫 / 音乐`，PRD 里只写了 2 个。

**建议目标规范**（待你确认）：
```
电影/片名 (年份) {tmdb-12345}/片名 (年份).ext
电视剧/片名 (年份) {tmdb-12345}/Season 01/片名 - S01E01.ext
动漫/片名 (年份) {tmdb-12345}/Season 01/片名 - S01E01.ext
```

---

## 5. 返工计划（M4 冻结，先补 M3）

| 阶段 | 内容 | 验收 |
|---|---|---|
| **M3.1** | renamer 改为**结构化重建**；支持"目录名+文件名"联合识别；`{tmdb-id}` 输出；4 类分区 | 真实语料 ≥95% 正确 |
| **M3.2** | 建**黄金语料库**：从 NAS 抓 200+ 真实文件名 + quark-name-map 键，作为回归基线 | 语料入库，CI 跑 |
| **M3.3** | 接入 `quark-name-map.json` 乱码名映射（消除对现有 bot 的功能回归） | 6 个已知乱码名全中 |
| **M3.4** | **落地器 ingest**：真正建目录 + 移动/改名，默认 `--dry-run` | dry-run 目录树经你确认 |
| **M3.5** | 转存器递归子目录 + 分页；光鸭接线修正；百度 CookieJar | 三网盘各 1 条真实链接成功 |
| **M3.6** | 凭据外置、`-race`、幂等去重、context+重试 | `go test -race` 绿，源码零凭据 |
| **M4** | TG Bot（解冻条件：M3.1–M3.6 全绿） | 端到端飞牛出海报 |

---

## 6. 需要你拍板的 3 个决策

**决策 A｜改名在哪一层做？**（架构分叉，影响最大）
- **A1 网盘侧改名**：调网盘 API 的 rename/move，在网盘内部整理目录结构。零流量、快，但三家 API 都要各写一套
- **A2 挂载点改名**：在 `/vol2/1000/CloudDrive/影视`（`fuse rw` 已确认可写）直接 `os.Rename`。一套代码通吃三家，但依赖 CloudFS 的 rename 实现，需先实测
- 我倾向 **A2 优先 + 实测兜底**，因为已确认挂载是 `rw`，且能统一三家逻辑

**决策 B｜命名规范定稿**
上面第 4 节的目标规范（带 `{tmdb-id}`、4 类分区）能否作为唯一标准？现存不规范目录要不要一并批量治理（需要 dry-run 报告）？

**决策 C｜动漫单独分类？**
`动漫/` 已存在且用 `S01E231` 连续编号（不按季分卷）。是走独立规则，还是并入 `电视剧/`？

---

**审计人**: 项目总指挥
**下一步**: M3.4 落地器（按 ADR-001 的 A1 方案，默认 dry-run）

---

## 附：M3.1–M3.3 收官记录

### 真实数据又推翻的 PRD 假设

| # | PRD 原假设 | 真实情况 |
|---|---|---|
| 1 | 一级分类两个（电影/电视剧） | 四个：电影/电视剧/**动漫**/音乐；且动漫占 95%（916/961）|
| 2 | 片名在文件名里 | **61%（587/961）文件名里没片名**，只有 `S01E231.2020.2160p...` |
| 3 | 中间层是 Season 目录 | 还大量存在**资源站分卷目录**（`001-100集`/`101-150 4K`/`合集篇`），飞牛不认 |
| 4 | 一个剧集包就是一部剧 | 内部混有**剧场版独立电影**（`剧场版/决战原始星.2026`）与**Specials 特典** |

### 两个只有真实数据能抄到的致命 Bug

1. **同一部剧被拆成多个目录**
   「吞噬星空」单集文件带 **2020/2024/2026 三种年份**，若从文件名取年份作目录名，
   会在飞牛里变成 3 部不同的番。
   → 定为约束：**剧集的年份只能来自目录名或 TMDB（剧集级），严禁取自单集文件名**
   → 回归测试：`TestFolderIdentityStable`

2. **30 个文件会被静默覆盖（数据丢失）**
   首轮碰撞检测：**29 组碰撞，将丢失 30 个文件**。三类根因：
   - `(v2)` 修正版与原版同名 → 加版本标记
   - `合集篇/011` 与正片 `S01E011` 撞号（它们是不同剪辑版）→ 标 NeedsReview，不猜
   - 同片多版本（2160p 国语 / 1080p HBOMax）→ 批量消歧加分辨率标签
   → 回归测试：`TestNoPathCollision`

### 构架级改变

- **减法清洗 → 结构化重建**：解析成字段 → 丢弃原串 → 用字段拼装。彻底消除 `流浪地球 ()` 残壳
- **逐文件决策 → 批量规划**：新增 `Disambiguate()`，落地前先算全量再消歧
- **猜 → 不猜**：新增 `NeedsReview`，无法确定归属宁可不入库，绝不覆盖

### 当前验收数据（961 条真实文件）

```
合计 961 | 可入库 943 (98.1%) | 待确认 18 | 碰撞 0 | 消歧补标签 2
空片名 0 | 残壳 0 | go test -race 全绿 | go build ./... 通过
```
待确认的 18 条全部是 `合集篇`（剪辑版），按设计不自动入库。

---

## M3.4: 落地器 (Lander) — 完成记录

**完成时间**: 2026-08-20
**交付物**: `internal/lander/lander.go` + `lander_test.go` + `cmd/fnosctl land` 命令

### 新增能力

| 能力 | 说明 |
|------|------|
| `PlanFromDir` | 遍历 rclone 挂载目录 → ParsePackage + NameMap + TMDB + Disambiguate → 生成目标路径 |
| `Execute` | 碰撞检测 → MkdirAll → os.Rename，默认 dry-run |
| `land` CLI | `fnosctl land <挂载根> [--source 子目录] [--cat 分类] [--execute]` |
| 路径分类自动剥离 | 路径第一段是分类(动漫/电影/...)时自动剥除，不重复 |
| 根目录散文件处理 | 无剧名目录时用扫描根目录名推断 |

### 修复

| 问题 | 根因 | 修复 |
|------|------|------|
| `W-万古至尊` 的 `W-` 前缀未剥离 | `reIndexPrefix` 正则只匹配 `字母+空格` | 改为 `^[A-Za-z][\s\-]+(\p{Han})`，只在后跟汉字时剥 |
| TMDB 错误被静默吞掉 | `_ = err` | 改为打印 stderr 警告 |
| 路径分类重复（电影/电影/后室） | 扫描根含分类目录 + BuildPath 又加分类 | 目标路径始终相对于 mountRoot |

### NAS 端到端实测（dry-run）

| 测试 | 结果 |
|------|------|
| W-万古至尊 (10集, 动漫) | `W-`剥离 + TMDB(tmdb-329125, 2026) + 路径正确 ✓ |
| 后室 (电影) | TMDB(tmdb-1083381) + 路径正确 ✓ |
| 鬼灭之刃 (动漫) | source=target, 幂等跳过 ✓ |
| 夸克动漫全量 (809文件) | 733规划 / 76跳过 / 0失败 ✓ |

### 测试

```
ok  fnos-enhance/internal/lander    1.017s  (5 tests, -race)
ok  fnos-enhance/internal/linker    (cached)
ok  fnos-enhance/internal/renamer   (cached)
```

### 架构限制

- **光鸭云盘 (GuangYa) 不支持**: CloudFS fuse 只读，CD2 gRPC-web API 未逆向
- **当前支持**: Quark rclone WebDAV 挂载 (mkdir ✓, rename ✓, write ✗)
- **下一步**: M3.5 Transfer 修复 → M3.6 工程加固 → M4 TG Bot 端到端

---

## M3.5–M3.6: 转存器重写 + 管道接通 — 完成记录

**时间**: 2026-08-19 深夜（用户离线，自主执行）
**验证方式**: 49 → 57 个测试 `-race` 全绿 + NAS 真实二进制行为验证 + 961 条语料回归

### 一、转存层重写（P0-4 / P0-6 / P1-1 / P1-2 / P1-7 / P2）

**根因修复：位置参数 → Config 结构体**

旧签名 `NewTransferor(quarkCookie, baiduCookie, guangyaClientID, guangyaClientSecret)`
有四个同类型 `string`，编译器无法发现「传进来的是 ClientID，而 `Transfer()` 只读 AccessToken」
——这是 P0-6「光鸭转存 100% 失败」的真正根因，不是简单的字段名写错。
现在改为 `Config{Quark, Baidu, GuangYa, ...}`，每家有独立 `Ready()`，
且 **构造期** `Validate()` 就拦住空凭据（NAS 实测：`错误: 三家网盘凭据均为空`）。

| 审计项 | 修复内容 | 回归测试 |
|---|---|---|
| P0-4 | 三家全部实现分页 + 递归到 `MaxDepth`（旧代码单页 50 条不递归，≈99% 剧集场景取不全） | `TestQuark_PaginationCollectsAllPages`（120 条翻 3 页）、`TestQuark_RecursesIntoSubdirs`、`TestBaidu_PaginationAndRecursion`、`TestGuangYa_PaginationAndRecursion` |
| P0-6 | AccessToken 优先 → 过期则用 RefreshToken+ClientID 自动刷新（JWT `exp` 提前 60s 判过期） | `TestGuangYa_ClientIDAloneIsNotReady`、`TestGuangYa_AutoRefreshesExpiredToken`、`TestJWTExpired` |
| P1-1 | 百度走 `cookiejar`，verify 下发的 `BDCLND` 跨请求携带；Jar 未捕获时用 `randsk` 兜底补写 | `TestBaidu_CarriesBDCLNDAfterVerify`（断言 list 请求真的带上了 BDCLND） |
| P1-2 | 从分享页正则抓真实 `bdstoken`（32 位 hex），不再硬写 `null` | `TestBaidu_ExtractsRealBDSToken`（断言 ≠ "null"） |
| P1-7 | `doJSON`/`doForm` 统一指数退避 + jitter；`retryable()` 只重试 429/408/5xx | `TestRetry_On429ThenSucceeds`、`TestRetry_ContextCancelStops`、`TestHTTPError_RetryableClassification` |
| P2 | 删除死代码 `ExtractGuangYaID`/`guangyaLinkRe`（现为 0 引用）；HTTP 方法显式传入，不再用 `data == nil` 隐式决定 GET/POST | `grep` 验证 0 命中 |
| P2 | `transfer` 包从 **0 测试** → **21 个测试** | — |

### 二、链接识别加固（P1-8 / P1-5）

**P1-8 修复过程中，我自己写的测试抓出了一个更深的漏洞。**

第一版只加了「前导边界 `[^A-Za-z0-9._-]`」，挡住了 `notquark.cn/s/evil`，
但 `TestDomainBoundary_RejectsLookalikes` 立刻失败并指出：

```
仿冒域名被误判: "https://evil.com/pan.quark.cn/s/abc" -> Type=quark ID=abc
```

因为 `/` 被当成合法边界字符，导致**域名出现在别人 URL 的路径里也被当成主机**。
最终边界规则改为 `(?:^|//|[^A-Za-z0-9._/-])`：排除单个 `/`，只放行 `//`（协议分隔符）。
三层防御：前导边界 + 子域显式化 `(?:[A-Za-z0-9-]+\.)*` + `/s/` 紧跟域名（挡 `pan.baidu.com.evil.com`）。

P1-5 幂等：`ParseLinks` 按 `(类型, ID)` 去重，并改为按文本出现顺序返回。
NAS 实测：3 个链接（含 1 个重复）→ 输出 2 条。

### 三、P0-1 管道断裂 — 本轮才真正闭环

排查中发现一个**之前所有记录都漏掉的事实**：`transfer` 和 `land` 是两个
互不相连的 CLI 子命令，`grep -rn "transfer\." internal/lander/` 结果为 **0**。
也就是说管道从来没接上，用户必须手工在中间等待并观察挂载。

新增 `internal/pipeline`，把「转存 → **等挂载可见** → 规划 → 落地」串成一条链，
新增 `fnosctl ingest` 子命令。链路中两个真实卡点被显式处理：

1. **转存提交成功 ≠ 挂载里可见**
   rclone WebDAV / CloudFS 都有目录缓存。转存完就直接 land 会扫到空目录、
   静默「成功」什么也没做。现在 `waitForAppear()` 轮询等待，并要求新条目
   连续两轮数量稳定才动手（避免目录还在陆续出现时就开始改名）。
   超时错误是**可操作**的，直接给出 `--dir-cache-time` / `vfs/forget` 线索。
2. **落地范围必须收窄到本次新增**
   `filterPlansByTopLevel()` 只对本次新出现的顶层条目落地。
   否则一次 ingest 会把挂载里所有命名不规范的旧数据一起改名。
   `TestPipeline_OnlyLandsNewEntries` 专门守这条。

前置防护（NAS 实测）：
- 挂载不可用时**先拦住，不去转存**——否则转存成功却无处落地
  （`错误: 挂载根不可用: /nope/not/mounted（挂载是否掉了？）`）
- 光鸭链接给出明确警告：可转存但**无法落地改名**（CloudFS 只读，见 ADR-001）

### 四、当前测试盘

| 包 | 测试数 |
|---|---|
| renamer | 14（含 961 条真实语料回归） |
| transfer | 21 |
| linker | 9 |
| pipeline | 8 |
| lander | 5 |
| **合计** | **57** |

`go build ./...` / `go vet ./...` / `go test ./... -race` 全绿。
961 条真实语料：自动入库 961 (100.0%)，需人工确认 0，零碰撞。

---

## 诚实的遗留问题（不要当成已完成）

**这一轮没有做到的事，必须写清楚，否则下一个人会以为管道能用了。**

1. **转存器从未对真实网盘 API 跑过。**
   21 个测试全是 `httptest` mock —— 它们验证的是我的**分页收敛、递归、
   Cookie 携带、重试、凭据校验**逻辑，**不能**证明与真实网盘 API 的字段兼容性。
   夸克/百度/光鸭的接口字段、签名要求、风控策略都可能与我的实现不符。
   **只有拿真实凭据跑一条真链接才能验证。**

2. **`ingest` 全链路从未真实跑通。**
   NAS 上只验证了「挂载检查」「光鸭警告」「凭据校验」这些前置分支，
   因为再往下就需要真实凭据 + 对用户网盘做不可逆写入。
   端到端 DoD 仍未达成：**一条真实链接 → 飞牛刮出海报**。

3. **光鸭落地依然无解。**
   961 条语料全部在光鸭上，而 CloudFS 挂载只读（ADR-001 已实测证伪）。
   CD2 gRPC-web(19798) proto 未逆向。这条腿是断的。

4. **用户的 tg-media-bot 容器里仍是失效的 TMDB Key `cc7790...`**，
   那边的刮削一直在静默失败。本项目已改为从密钥文件读，但**旧 bot 没修**。

5. **M4 (TG Bot) 仍应冻结**，直到 1 和 2 用真实凭据验证通过。

### 下一步唯一有意义的动作

不是继续写代码，而是拿一条真实链接做一次端到端：

```bash
export QUARK_COOKIE='...'
# 先 dry-run 看规划
/tmp/fnosctl ingest "<真实夸克链接>" --mount /vol02/1000-1-a92fbdbc/影视 --source 0_待整理
# 确认无误后
/tmp/fnosctl ingest "<真实夸克链接>" --mount /vol02/1000-1-a92fbdbc/影视 --source 0_待整理 --execute
```

**网盘改名不可逆**，所以 `--execute` 需用户本人确认后再跑。

---

## 真实链接验证 — 首次打通真实网盘 API（2026-08-20）

**输入**: 用户提供的真实夸克分享 `https://pan.quark.cn/s/d4dc6878059c#/list/share`

这是 REVIEW.md 上一节标记为「未做到」的那个缺口。用真实链接一跑，
**立刻抓出 3 个 mock 测试全部漏掉的问题**，其中 1 个是崩溃。

### 一、转存层：字段名全部正确（首次真实验证）

意外发现：夸克 `share/sharepage/token` 与 `detail` **无 cookie 也能读公开分享**。
因此原先「构造期硬拦空凭据」是过度限制 —— dry-run 预览不该要凭据。
改为 `Validate()` 只填默认值，新增 `RequireCredentials()` 只在 `--execute` 前校验。

实测我的二进制对真实 API：
```
✓ 夸克 | 顶层 1 项 | 递归共 20 个文件
```
`data.list[].fid / file_name / dir / file_type / share_fid_token`、
`metadata._total` 与实现完全一致；分页、递归（MaxDepth 3）均正确。

真实分享结构：
```
Z - 罪 - A/                                       ← 顶层
  01.4k.mp4 … 10.4k.mp4                          ← 版本1，裸集号
  4K/ S01E07~E10.mp4
      2026.2160p.HDR.60fps.DDP5.1.S01E01~E06.mkv ← 版本2
```

### 二、崩溃 #1：`pickBest` 全率否决 → `Search` 返回 `(nil, nil)` → panic

```
panic: runtime error: invalid memory address or nil pointer dereference
  renamer.(*TMDBClient).Enrich  tmdb.go:277
```

`pickBest()` 在所有候选都被年份校验否决时返回 `nil`，`Search` 却
把 `(nil, nil)` 交给调用方，`Enrich` 直接 `r.ID` → 崩。

**这是契约违反，不是边界情况**：57 个测试全用 mock 喂了能匹配的结果，
从没喂过"有候选但全被否决"。修复：`Search`/`searchLang` 保证
永不返回 `(nil, nil)`；`Enrich` 加双重 nil 兜底。
回归测试 `TestSearch_NeverReturnsNilNil`、`TestEnrich_NoMatchReturnsErrorNotPanic`。

### 三、崩溃 #2（潜伏）：nil 进缓存会绕过所有 nil 检查

```go
if r, ok := c.cache[key]; ok { return r, nil }   // r 若为 nil → (nil, nil)
```
缓存命中路径在所有 nil 检查**之前**，一旦 nil 入缓存就重新打开 panic 通道。
修复：命中条件加 `&& r != nil`，写入统一走 `remember()`（nil 绝不入缓存）。
回归测试 `TestSearch_CacheNeverYieldsNilNil`（人为塞 nil 进缓存）。

### 四、裸集号无法识别 → 10 个文件映射到同一路径

`01.4k.mp4` … `10.4k.mp4` 全部被判为「电影，无集号」，
10 个文件目标路径完全相同。**碰撞检测拦住了**（标为待确认，未静默覆盖），
安全网有效，但功能缺口是真的：17 个文件只有 8 个能自动入库。

新增 `bareEpisode()` 兜底，三道防误伤门：
1. 只收 1–3 位数字（排除 `2026` 年份、`1080`/`2160` 分辨率）
2. 排除常见低分辨率值 360/480/540/576/720
3. **数字后的残余必须为空或纯技术标记** —— 这条挡住 `007 James Bond.mkv`
   （残余 ` James Bond` 不是技术标记 → 拒绝，不会误判成第 7 集）

结果：可入库 8 → **13**。剩 4 个待确认是真实歧义
（root `07.4k.mp4` 与 `4K/S01E07.mp4` 同集同扩展名、无区分标记），
交人工判断是正确行为。

**961 条黄金语料仍 100%、零碰撞** —— 这个兜底最容易误伤，回归是硬门槛。

### 五、TMDB 失败结果未缓存 → 同片名发 17 次重复请求

只有成功结果进缓存，失败每次都重新请求。961 文件批量跑会直接撞限流。
修复：新增 `negative` 负缓存（key → 失败原因）；CLI 侧同一片名只警告一次。
实测：警告 17 行 → 1 行，HTTP 请求 17 次 → 2 次（中文 + 英文 fallback）。
回归测试 `TestSearch_CachesNegativeResults`（10 次查询断言 ≤2 个请求）。

### 六、TMDB 查不到《罪》(2026) 是正确行为，不是 bug

分享名 `Z - 罪 - A` 是刻意混淆的（盗版规避关键词），TMDB 无此条目。
`pickBest` 拒绝套用「犯罪心理」「罪恶黑名单」等无关结果 —— 这正是设计意图。
片名残留的 ` - A` 后缀**没有修**：只有一个样本，无法判断
「`X - 片名 - Y` 是通用约定」还是「这家上传者的习惯」。
按「不猜」原则留给 NeedsReview 标记，不凭单一样本发明规则。

### 测试盘：57 → 62

| 包 | 测试数 |
|---|---|
| renamer | 19（+5：nil 契约 ×3、负缓存 ×1、裸集号回归） |
| transfer | 22（+1：无 cookie dry-run） |
| linker | 9 |
| pipeline | 8 |
| lander | 5 |

### 仍未做到

- **转存的 `save` 步骤仍未验证**：需要真实 QUARK_COOKIE，用户凭据库里没有
  （只有 `baidu-pan` 和 `guangya-dev`）。列举链路已真实验证，写入链路没有。
- **`ingest` 全链路仍未真实跑通**：缺 cookie，且需对用户网盘做不可逆写入。
- 端到端 DoD（真实链接 → 飞牛刮出海报）**仍未达成**。M4 保持冻结。
