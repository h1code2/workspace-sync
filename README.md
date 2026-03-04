# workspace-sync

轻量级实时文件同步工具（Go），用于将**发送端目录**持续同步到**接收端目录**，并在可控策略下保持一致。

---

## 适用场景

- 开发机 → 运行机（持续同步工作目录）
- 单向主从同步（推荐）
- 需要断线恢复、重试、周期修复的轻量文件同步

---

## 核心能力

- 实时监听文件变更（fsnotify）
- 启动全量快照 + 周期重同步（`--resync`）
- 分块传输（大文件低内存峰值）
- ACK + 重试机制（提升可靠性）
- 断点续传（`resume_from`）
- sender worker 并发发送（`--send-workers`）
- 协议能力协商（`hello/protocol/caps`）
- 指标日志（发送/重试/失败/吞吐等）
- 本地冻结目录（`.nosync`）

---

## 快速开始

### 1) 接收端（示例：macOS）

```bash
workspace-sync-darwin-arm64 -r -d /path/to/recv -p 17077 --token "xxx"
```

### 2) 发送端（示例：Linux）

```bash
workspace-sync-linux-amd64 -s -d /path/to/send --peer 1.2.3.4:17077 -p 17077 --token "xxx"
```

> 推荐生产使用：**单向 send -> receive**。`both` 模式可用，但不建议在复杂冲突场景直接生产启用。

---

## 常用参数

### 基础

- `--mode send|receive|both`（或 `-s` / `-r`）
- `--dir, -d`：同步目录
- `--peer`：发送目标（send/both 必填）
- `--listen, -l, -p`：接收监听地址/端口
- `--token, -t`：共享鉴权 token

### 同步节奏

- `--debounce`：事件去抖（默认 `400ms`）
- `--resync`：周期重同步（默认 `60s`，`0` 关闭）

### 可靠性与性能

- `--chunk-size`：分块大小（字节）
- `--ack-timeout`：ACK 超时时间
- `--max-retries`：发送最大重试次数
- `--send-workers`：并发发送 worker 数
- `--enable-resume`：是否启用断点续传
- `--metrics-interval`：指标日志输出周期

### 过滤

- `--exclude <glob>`（可重复）

---

## `.nosync` 目录冻结

在接收端或发送端目录内创建 `.nosync`，可冻结该目录子树：

```bash
touch some/dir/.nosync
```

行为：

- **接收端**：跳过该子树的事件应用与 snapshot prune（本地文件不被收敛覆盖）
- **发送端**：跳过该子树发送（降低无效流量）

恢复同步：

```bash
rm some/dir/.nosync
```

> 升级提示：旧标记 `.stop` 已更名为 `.nosync`。

---

## 后台运行

```bash
workspace-sync ... start
workspace-sync status
workspace-sync stop
```

默认日志（Linux）：

- `~/.cache/workspace-sync/workspace-sync.log`

---

## 发布

推送 tag 可触发自动发布（仓库启用 release workflow 时）：

```bash
git tag v0.3.0
git push origin v0.3.0
```
