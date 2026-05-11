# backend

视频聚合站的 Go 后端。提供三件事：

1. 多家网盘统一抽象（夸克 / 115 / PikPak / 联通沃盘 / OneDrive）
2. 视频元数据目录（SQLite）+ 扫描 + teaser 预生成
3. REST API（前台）+ 管理后台 + 直链代理
4. 标签池、视频隐藏、按网盘统计和详情页来源网盘类型展示能力

## 目录

```
cmd/server/main.go          入口
internal/
  config/                   YAML 配置
  catalog/                  SQLite 元数据
  drives/
    iface.go                Drive 接口
    quark/                  夸克（自己实现，参考 OpenList quark_uc）
    p115/                   115（壳子 + SheltonZhu/115driver）
    pikpak/                 PikPak（自己实现，参考 OpenList pikpak）
    wopan/                  联通沃盘（壳子 + OpenListTeam/wopan-sdk-go）
    onedrive/               OneDrive（OpenList 在线续期 + Microsoft Graph 文件接口）
  scanner/                  扫目录 → 落库
  preview/                  ffmpeg 抽封面和生成多段 teaser
  proxy/                    /p/stream/*、/p/preview/* 代理
  auth/                     管理员 session
  api/                      REST 路由
config.example.yaml         配置模板
```

## 开发环境（Windows）

本仓库假设工具都装在用户目录，不需要管理员权限。

```
C:\Users\<you>\tools\
  go\bin\go.exe             Go 1.23+
  ffmpeg\bin\ffmpeg.exe     任意 ≥ 4.x 版本
```

并加到 `PATH`。

### 第一次启动

Git Bash / WSL 环境推荐从仓库根目录启动完整开发环境：

```bash
npm install
./start.sh               # 默认前端 production preview，无热更新
```

需要前端开发热更新时再用 `FRONTEND_MODE=dev ./start.sh --restart`。

PowerShell 下可以分两个终端手动启动，后端命令如下：

```powershell
cd F:\VideoProject\backend
go run ./cmd/server
```

首次启动会在当前目录创建：

- `config.yaml`（从 `config.example.yaml` 复制）
- `data/video-site.db`
- `data/previews/`

默认监听 `127.0.0.1:9192`，默认管理员 `admin / admin123`（务必在 `config.yaml` 里改）。如果本地已有旧的 `config.yaml`，请确认 `server.listen` 与前端代理端口一致。

### 连接前端

`vite.config.ts` 已经把 `/api`、`/p`、`/admin/api` 代理到 `127.0.0.1:9192`。

```
npm run build       构建前端静态资源
npm run preview     前端 9191，无热更新
go run ./cmd/server 后端 9192
```

## 添加一个盘

推荐在前端管理后台 `/admin/drives` 新增网盘。保存后会立即挂载并触发扫描；视频结果可在 `/admin/videos` 按网盘查看，每页 100 条，页面会同时显示各网盘 Teaser 已生成、待生成、失败数量。

也可以直接调用后端接口：

1. 登录管理后台：`POST /admin/api/login` body `{"username":"admin","password":"admin123"}`
2. 新建盘：`POST /admin/api/drives`
   ```json
   {
     "id":   "my-quark",
     "kind": "quark",
     "name": "我的夸克盘",
     "rootId": "0",
     "scanRootId": "0",
     "credentials": {
       "cookie": "粘贴浏览器 F12 复制的 pan.quark.cn Cookie"
     }
   }
   ```
3. 手动触发扫描：`POST /admin/api/drives/my-quark/rescan`

各网盘的凭证字段：

| kind   | credentials 字段                                              |
|--------|---------------------------------------------------------------|
| quark  | `cookie`                                                      |
| p115   | `cookie`（形如 `UID=...; CID=...; SEID=...; KID=...`）         |
| pikpak | `username`、`password`，可选 `refresh_token`、`captcha_token`、`device_id`、`platform`、`disable_media_link` |
| wopan  | `access_token`、`refresh_token`，可选 `family_id`              |
| onedrive | `refresh_token`，可选 `access_token`、`api_url_address`、`region`、`is_sharepoint`、`site_id` |

OneDrive 按 OpenList 默认方式调用 `https://api.oplist.org/onedrive/renewapi` 在线刷新 token，不需要配置 Azure 应用的 `client_id` / `client_secret` / `redirect_uri`。OpenList 代刷得到的 refresh token 可以直接填到本项目。普通 OneDrive 的 `rootId` / `scanRootId` 可填 `root`；SharePoint 文档库需要额外设置 `is_sharepoint=true` 和 `site_id`。

## 文件名约定

扫描器按以下顺序解析文件名：

1. `[tag1,tag2] 标题 - 作者.mp4`
2. `[tag1,tag2] 标题.mp4`
3. `标题 - 作者.mp4`
4. `标题.mp4`

标签分隔符支持 `, ， 、` 和空格。解析结果会和系统标签池匹配，常见番号类噪声会归并到 `AV` 等系统标签，避免把每个番号都变成独立标签。解析结果可在管理后台覆盖。

## 管理能力

- `/admin/drives`：新增、编辑、删除网盘，触发扫描。
- `/admin/videos`：按网盘筛选视频，每页 100 条分页，查看各网盘 Teaser 统计，编辑标题/作者/分类/标签，单条或全量重生 teaser。
- `/admin/tags`：新增标签并用标签规则自动匹配已有视频。
- 播放页视频信息会展示来源网盘类型；同时提供“不再展示”，点击后会把视频标记为全局隐藏。隐藏视频不会再出现在首页、列表、搜索、相关推荐和详情接口中。目前没有管理后台恢复入口，如需恢复可把数据库里对应视频的 `hidden` 字段改回 `0`。

## Teaser 生成

scanner 扫到新视频会把 `(driveID, videoID)` 丢进 worker 队列。worker 会先用 `ffprobe` 探测时长，再用 `ffmpeg` 抽封面和生成无声 teaser：

```
ffmpeg -ss <起点> -headers "UA/Cookie/Referer" -i <直链> \
       -t 3 -an -vf scale=480:-2 -c:v libx264 -preset veryfast -crf 28 \
       -movflags +faststart -y <local>.mp4
```

当前策略是每段固定 3 秒；30 秒以下最多 3 段，30 秒及以上固定 4 段；长视频在 20% 到 80% 区间均匀取段。优先把 teaser 上传回网盘的 `previews/` 目录；失败时保留本地 `data/previews/<videoID>.mp4` 作为兜底。

服务启动或网盘重新挂载时，如果 Teaser 开关已开启，后端会把历史 `pending` 任务重新入队，避免重启后长期停在“待生成”。OneDrive 直链生成 teaser 时可能触发 Microsoft 429 限流；后端会识别这类错误并让当前网盘进入冷却期，保留任务为 `pending`，避免连续请求触发更严重限流。

前端卡片的 `previewSrc` 统一指向 `/p/preview/<videoID>`，后端自动选择网盘代理或本地文件。

## 验证

```bash
# 前端，在仓库根目录执行
npm run lint
npm run build
node --test tests/previewIntent.test.ts

# 后端，在 backend/ 执行
go test ./... -count=1
```

## 部署到 Linux

```bash
# 交叉编译
GOOS=linux GOARCH=amd64 go build -o video-server ./cmd/server

# 目标机
sudo apt install ffmpeg
scp video-server user@host:/opt/video-site/
ssh user@host
cd /opt/video-site
cp config.example.yaml config.yaml
# 改密码、监听地址
./video-server
```

配 systemd + nginx 反代到 `/` 和 `/api`、`/p`、`/admin`。
