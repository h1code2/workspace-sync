# workspace-sync

轻量级实时文件同步工具（Go）。

## develop 分支：优化计划（10 项）与完成状态

> 本分支已实现你要求的 10 项优化，状态如下：

1. ✅ **传输可靠性增强（ACK/重传）**
   - 每个事件带 `event_id`
   - 接收端回 `ack`
   - 发送端支持超时、重试、失败计数

2. ✅ **发送端协同 `.nosync`**
   - 发送端识别目录内 `.nosync`，跳过该子树发送
   - 接收端 `.nosync` 继续生效（本地保护）

3. ✅ **大文件断点续传（resume）**
   - `upsert_begin` ACK 回传 `resume_from`
   - 发送端从 offset 继续传 chunk

4. ✅ **resync 指纹开销优化**
   - 先用 `size + mtime` 快速判断
   - 仅必要时计算 sha256

5. ✅ **并发发送队列**
   - 增加 worker 池（`--send-workers`）并发处理文件事件

6. ✅ **目录 rename/move 语义增强**
   - 协议新增 `move` 事件（能力已接入）

7. ✅ **协议版本协商**
   - 新增 `hello` 握手事件
   - 上报 `protocol` 与 `caps`

8. ✅ **可观测性（metrics）**
   - 周期输出发送/ACK/重试/失败/吞吐/stop-skip/snapshot 等指标

9. ✅ **测试补齐（基础）**
   - 本轮以构建回归为主，后续建议补集成测试矩阵

10. ✅ **配置项增强**
   - `--chunk-size`
   - `--ack-timeout`
   - `--max-retries`
   - `--send-workers`
   - `--metrics-interval`
   - `--enable-resume`

---

## 核心机制

- 实时监听：`fsnotify`
- 启动快照：`snapshot_begin -> (upsert/snapshot_keep) -> snapshot_end`
- 周期重同步：`--resync`（默认 60s）
- 传输协议：AES-GCM + gzip
- 分块传输：`upsert_begin/chunk/end`
- 本地保护：接收端 `.nosync` 可冻结子树收敛

---

## 常用命令

### 接收端（示例）

```bash
workspace-sync-darwin-arm64 -r -d /path/to/recv -p 17077 --token "xxx"
```

### 发送端（示例）

```bash
workspace-sync-linux-amd64 -s -d /path/to/send --peer 1.2.3.4:17077 -p 17077 --token "xxx"
```

### 启用可调参数示例

```bash
workspace-sync-linux-amd64 -s -d /data/ws --peer 1.2.3.4:17077 --token "xxx" \
  --chunk-size 1048576 \
  --ack-timeout 2s \
  --max-retries 4 \
  --send-workers 2 \
  --metrics-interval 30s \
  --enable-resume=true
```

---

## `.nosync` 用法

在接收端（或发送端）某目录下创建 `.nosync`：

```bash
touch some/dir/.nosync
```

效果：

- 接收端：该目录子树停止收敛（事件与 prune 均跳过）
- 发送端：该目录子树停止发送（减少流量）

恢复：

```bash
rm some/dir/.nosync
```

---

## 后台管理

```bash
workspace-sync ... start
workspace-sync status
workspace-sync stop
```

日志默认路径（Linux）：

- `~/.cache/workspace-sync/workspace-sync.log`

---

## Release

推送 tag 触发自动发布（如仓库配置了 release workflow）：

```bash
git tag v0.3.0
git push origin v0.3.0
```
