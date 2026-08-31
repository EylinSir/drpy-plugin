# drpy-plugin

drpy/TVBox 插件库：用 Go 编写并交叉编译为跨平台二进制，为 drpys 客户端提供媒体代理和请求代理能力。含两个独立 Go 模块，互不依赖。

## 目录结构

- `mediaProxy/` — 核心项目：多线程分片下载的视频代理服务（Range 流式播放、base64 防 SNI 阻断、4 小时缓存、auth 认证）。Go 1.21，模块名 `MediaProxy`。
  - `proxy.go` — 全部主逻辑（入口、路由、并发下载）
  - `base/` — 本地包（导入路径必须写 `MediaProxy/base`）：`client.go` HTTP 客户端、`emitter.go` 流式发射器
  - `static/index.html` — 通过 `//go:embed static` 嵌入二进制，改了要重新编译
  - `docs/README_DEV.md` — **改代理逻辑前必读**（Range/416/429 处理、调试流程、打包闭环）
- `req-proxy/` — 轻量 HTTP 请求转发代理（spider 网络请求用）。Go 1.25，依赖 `imroc/req/v3`。`main.mjs` 是 Node 启动器，负责按平台拉起对应 Go 二进制。
- `golang.md` — 未整理的 Go 学习笔记。

## 常用命令

```bash
# mediaProxy（在 mediaProxy/ 目录下）
go run proxy.go -port 5575 -debug   # 本地调试，默认端口 57574、auth 默认 drpys
go build ./... && go vet ./...      # 编译/静态检查
go test ./...                       # 测试（proxy_test.go 用 httptest 模拟源站，含故障注入）
make build                          # 当前平台（输出 build/）
.\scripts\build_goproxy.ps1         # 交叉编译 Android arm/arm64 → goProxy/
.\scripts\update_jar.ps1            # 把 goProxy 二进制打进 custom_spider.jar 并重算 .md5（需 7z 在 PATH）

# req-proxy（在 req-proxy/ 目录下）
go build ./...
powershell -File build.ps1          # 三平台编译（Windows/Linux amd64 + Android arm64，NDK 路径硬编码在脚本顶部）
```

## 架构与改动规则

- mediaProxy 代理核心集中在 `ConcurrentDownload`、`ProxyRead`、`ProxyWorker` 三个函数，改动需小心 channel/buffer 读写与 `context.WithCancel` 生命周期（播放器断开时必须 cancel，否则协程泄漏）。
- Range 处理约定：网盘返回 **416 不是致命错误**（跳出分片循环平滑结束）；429/503 需退避重试并在 `handleGetMethod` 限制最大线程（16）。
- 认证：所有 API 必须校验 `auth` 参数与启动时 `-auth` 一致。

## 关键 gotcha：二进制是发布产物，直接提交在仓库里

`custom_spider.jar`、`custom_spider.jar.md5`、`goProxy/*`、`req-proxy/dist/*`、`mediaProxy/build/*` 都被 git 跟踪（客户端直接下载它们），`.gitignore` 的 `*.exe` 规则对已跟踪文件无效。因此：

1. 改完代理代码后的发布闭环 = 编译 Android 二进制 → `update_jar.ps1` 打包 → 提交更新后的 jar + md5 + 二进制。
2. `custom_spider-origin.jar` 是原始未修改的 jar，仅作备份，不要往里打包。
