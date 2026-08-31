# mediaProxy (goProxy) 开发与调试指南

本文档详细介绍了 drpy-plugin 中 `mediaProxy` (由 Go 编写的视频代理服务，即 `goProxy`) 的完整开发、测试、调试及编译打包流程。

## 0. 下载引擎架构（V1.1.0 起）

代理采用 **leader-follower 流式分段** 架构（对标 IDM/NDM/aria2 的动态分段思想）：

- **无独立探测请求**：常规 GET 的冷启动由 leader（首个分段请求，从播放器请求的偏移开始）直接承担。leader 的响应头（206 Content-Range）确定文件总大小后，立即向播放器写响应头并开始**边下边转**（按 128KB 数据块流水线入队，不整段缓冲）。首字节耗时 ≈ 1 个源站 RTT。
- **热缓存路径**：60 秒内 seek/重连直接用缓存的总大小写播放器响应头（0 额外往返），leader 同步开播。探测请求仅剩两个场景使用：HEAD 请求、冷缓存的后缀 Range（`bytes=-N`）。
- **followers**：其余线程按 2MB 分段（`-seg`/URL `size` 可调）顺序认领后续区间，各自流式下载切块入有序队列。
- **有序队列 + 双重背压**：播放器消费速度通过 `io.Pipe` 传导；预读窗口（`-window`，默认 16MB）用令牌信号量限制"已下载但未消费"的数据量，内存占用 ≈ 窗口 + 线程数×0.5MB。
- **失败隔离**：分段级退避重试（0.5s→8s 指数 + 抖动，429/503 尊重 Retry-After），重试期间其他分段继续下载；仅当分段耗尽 5 次尝试才断流，播放器会以 Range 重连续传。
- **416 = 平滑 EOF**：源站认为到达文件尾时按正常结束处理，不再中断整条流。
- **读空闲看门狗**：单分段 30 秒无数据强断重试，取代旧版全局 10 秒读超时的误杀。
- **传输层**：连接池 `MaxIdleConnsPerHost=64`（Go 默认 2，否则 16 线程并发会握手风暴）、HTTP/2、DNS 缓存 5 分钟。
- **降级**：源站不支持 Range（对 start>0 返回 200）自动单流转发。

## 1. 环境准备

确保您的本地开发环境已安装以下工具：
- **Go**: 建议 1.21 或更高版本（用于编译 `proxy.go`）
- **Python**: 3.x（用于运行模拟播放器及测试脚本）
- **PowerShell**: Windows 环境默认自带（用于执行编译及打包脚本）
- **Git**: 用于版本控制
- **UPX**: 可选，用于压缩编译后的可执行文件（在 Android 环境下能大幅缩小体积）

## 2. 本地启动代理服务

在开发阶段，我们直接使用 Go 命令在本地启动代理服务。服务默认运行在 `5575` 端口。

```powershell
cd mediaProxy
go run proxy.go -port 5575
```

如果需要查看更详细的调试信息（例如每个分片的下载情况、错误日志），请添加 `-debug` 参数：

```powershell
go run proxy.go -debug -port 5575
```

## 3. 测试与模拟播放器请求

在本地服务启动后，需要验证代理是否能正确处理视频的完整播放以及拖拽操作（即 HTTP Range 请求）。

### 3.1 准备测试数据

在 `mediaProxy` 目录下创建一个名为 `playinfo.txt` 的文件，将你需要测试的视频直链（例如迅雷等网盘解析出的原始播放链接）粘贴进去。

### 3.2 运行测试脚本

我们编写了两个 Python 脚本来模拟不同场景的测试：

1. **基础流程测试 (`test_proxy.py`)**: 
   该脚本模拟了完整下载、拖拽到中部、拖拽到尾部以及拖拽后继续播放四个场景。
   ```powershell
   python test_proxy.py
   ```
   **预期结果**: 各个场景返回 `206 Partial Content`，并且能正确下载对应字节数的数据。

2. **播放器模拟测试 (`test_player_sim.py`)**:
   该脚本更贴近实际播放器的行为，通过读取 `playinfo.txt` 中的链接，发起不同 `Range` 的请求，并打印响应头，方便检查 `Content-Range` 和 `Content-Length` 是否正确。
   ```powershell
   python test_player_sim.py
   ```

## 4. 调试错误与代码修改

当遇到播放截断、无法拖拽等问题时，通常是因为 HTTP Range 处理或并发控制不当。以下是常见的排查点：

- **观察 Debug 日志**: 
  检查 `go run proxy.go -debug` 输出的日志，特别关注 `statusCode`、`Range` 以及 `ProxyRead/ProxyWorker` 相关的报错。
- **416 错误处理**: 
  网盘（如迅雷）在请求到达文件末尾时常返回 `416 Range Not Satisfiable`。这**不是**一个致命错误，代理不应该直接中断。正确的做法是跳出当前分片的下载循环，平滑结束。
- **429/503 频率限制**: 
  多线程并发请求可能触发服务器的防刷机制。需要加入退避重试（Retry Backoff）机制，并在 `handleGetMethod` 中限制最大线程数（如限制为 16）。
- **Context 与协程泄漏**: 
  使用 `context.WithCancel` 控制并发任务的生命周期。当主请求结束（如用户关闭播放器）时，通过 `cancel()` 通知所有 worker 退出，避免后台持续下载导致内存和连接泄漏。

> **修改建议**: 所有代理逻辑的核心在 `engine`（分段调度 `claim`/`worker`、流式下载 `streamBody`、按序泵出 `pumpTo`）及 `handleGetMethod` 中，修改时需小心 channel 读写顺序与 `context` 生命周期。

### 4.1 自动化测试

仓库内置 `proxy_test.go`，通过 httptest 模拟源站（可注入延迟/限速/429/416/错误 Content-Range 等故障）覆盖以下契约：

```powershell
cd mediaProxy
go test ./...        # 正确性、秒级起播(免探测)、多线程生效、故障重试、降级、seek泄漏
```

关键断言：冷启动源站请求数恰好等于分段数（无探测请求）、首字节耗时 ≈ 1 个 RTT、源站峰值并发 ≥ 2、中断后协程数回归基线。改动引擎逻辑后必须跑通。

## 5. 编译 Android 二进制文件

在本地测试通过后，需要将 `proxy.go` 交叉编译为 Android 平台可执行文件。

在 `mediaProxy` 目录下，直接运行提供的 PowerShell 脚本：

```powershell
.\scripts\build_goproxy.ps1
```

**脚本主要工作**:
1. 设置环境变量 `GOOS=linux`，分别针对 `GOARCH=arm` (32位) 和 `GOARCH=arm64` (64位) 进行编译。
2. 禁用 CGO (`CGO_ENABLED=0`)，移除调试信息 (`-ldflags="-s -w"`) 以减小体积。
3. 输出文件存放至 `goProxy/goProxy-arm` 和 `goProxy/goProxy-arm64`。
4. (可选) 如果系统中存在 `upx` 命令，脚本会自动对编译结果进行 UPX 压缩。

## 6. 打包 custom_spider.jar

编译好的 Android 可执行文件最终需要打包进 `custom_spider.jar` 中的 `assets` 目录下供 TVBox 等客户端使用。

在 `mediaProxy` 目录下，运行打包脚本：

```powershell
.\scripts\update_jar.ps1
```

**脚本主要工作**:
1. 将 `goProxy` 目录下的 `goProxy-arm` 和 `goProxy-arm64` 添加或更新到上一级目录的 `custom_spider.jar` 的 `assets/` 路径中。
2. 自动计算更新后 `custom_spider.jar` 的 MD5 值。
3. 将生成的 MD5 值写入 `custom_spider.jar.md5` 文件中。

至此，整个开发、测试、编译、打包的闭环流程完成。你可以将生成的 `custom_spider.jar` 和 `custom_spider.jar.md5` 发布到你的源中供用户更新。
