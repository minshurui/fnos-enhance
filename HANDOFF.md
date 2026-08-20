# 交接 — fnos-enhance（飞牛增强层）

## 一句话
TG链接 → 识别 → 转存 → 改名 → 飞牛能刮。Go，零外部依赖。

## 位置
```
📍 代码 /root/.pi/agent/skills/vibevibe-advanced/projects/fnos-enhance/
📍 repo github.com/minshurui/fnos-enhance (main, 最新 4eb270b)
📍 NAS 二进制 /tmp/fnosctl (x86_64)
📍 经验 skills/minshurui-rules/SKILL.md (lesson 30-34 + tg-bot 档案)
```

## 凭据（勿明文，用别名）
```
📍 NAS SSH → secret_run entries=["nas-ssh"] + sshpass（ssh_saved_exec 密钥登录会失败）
📍 TMDB → NAS:~/.config/fnos-enhance/tmdb.key（secret store 里的 tmdb 条目已失效 401）
📍 光鸭 → NAS:~/.config/fnos-enhance/guangya.json（access/refresh/client_id）
📍 夸克官方 → skills/quarkclouddrive + QODER_IDE=1
📍 bot 真实密钥 → NAS:/root/YiMao/.env（root 所有，改后必须 sudo docker compose）
```

## ✅ 已完成
```
✅ 识别 961 真实路径 100% 无碰撞，99 个测试全绿
✅ 光鸭原生 API 后端 --guangya，不再需要 Alist（协议移植自 AlistGo/alist）
✅ 夸克官方 OAuth 转存 --official，真跑成功（30 项）
✅ 订阅追更 sub add/list/check/off，真链接验证过
✅ bot 的 TMDB key 401→200 已修
```

## 🔧 待做（按优先级）
```
🔧 M5 DoD 未达成：ingest 全链路从没一次跑通过（各段单独都验过了）
🔧 从没执行过真实改名，全是 dry-run —— 云端改名不可逆，必须你本人点 --execute
🔧 bot 接 fnosctl：只换掉 autoRename()，其余(搜索/回调/Emby/多盘)不动
🔧 夸克落点没定：现在进「来自：分享」，要不要 --to-pdir-path 固定
🔧 sub 定时跑：加 systemd timer 或 cron（--loop 已实现但没部署）
```

## ⚠️ 坑
```
⚠️ 光鸭必须每端点限流 500ms —— 否则返回「200+success+无 list」被当空目录，静默丢文件
⚠️ 验证别用自己的实现验自己 → 拿 CloudFS 只读挂载 find 当第三方基准
⚠️ secret_run 会把真数字当密钥打码：2022 年份被打成 [REDACTED]，不是 bug
⚠️ env | grep 会被安全拦截，一次只探一个变量
⚠️ sudo -S 要写 -p '' 否则吞掉第一行输出
⚠️ NAS 不支持 scp → base64 | ssh 'base64 -d'
⚠️ 覆盖运行中的二进制报 Text file busy → 先 rm
⚠️ 片名≤2字且无年份一律拒绝 TMDB 匹配（宁可跳过，不能认错）
⚠️ dry-run 不能有副作用（不写 Seen/LastCheck），否则预演一次真跑就不转存了
⚠️ bot 的 Alist 5245 / Emby 8097 / panlink 2091 / gost 2088 全是挂的，只有搜索 3001 活着
⚠️ 代理 2081 别碰，隔壁在修
⚠️ DeepSeek 已放弃（余额不足 402），aiIdentify 是死代码
⚠️ 不删任何东西（alist-probe 容器只停不删）
```

## ⚠️ 3 个安全问题（我没改，等你定）
```
⚠️ bot token 明文打进日志 → docker logs 就能劫持 bot
⚠️ PANSOU_PASSWORD 和 NAS sudo 密码相同（复用）
⚠️ ALIST_PASSWORD=admin
```

## 常用命令
```bash
# 光鸭落地（原生 API，不需 Alist）
export TMDB_API_KEY_FILE=~/.config/fnos-enhance/tmdb.key
/tmp/fnosctl land "/影视" --guangya --cat=电影 --source=电影     # dry-run
# 加 --execute 才真改名（不可逆，需你确认）

# 夸克官方转存
export QUARK_SKILL_DIR=/root/.pi/agent/skills/quarkclouddrive QUARK_AGENT_ENV='QODER_IDE=1'
/tmp/fnosctl transfer "<链接>" --official --execute

# 订阅追更
/tmp/fnosctl sub add "<链接>" --title "剧名" --cat 电视剧
/tmp/fnosctl sub check --execute       # 首次只建基线，不倒灌历史
```

## 下一步建议
先把 `ingest` 全链路在夸克上跑通一次（dry-run→确认→execute→看飞牛出海报），
这是 PRD 里唯一还没达成的 DoD。别再加新功能。
