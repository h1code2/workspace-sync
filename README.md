# workspace-sync

`workspace-sync` 是一个面向开发与运维场景的轻量级目录同步工具。

它的目标很明确：

- 把**发送端目录**的变更持续同步到**接收端目录**
- 在网络波动、漏事件、手工误改等情况下仍可自动收敛
- 提供可控的“局部冻结”（`.nosync`）能力，避免你本地工作目录被强制覆盖

---

## 1. 软件作用（解决什么问题）

在多机协作时，常见痛点是：

1. 实时同步不稳定：偶发网络抖动就可能漏文件
2. 大文件占内存：一次性读写导致峰值过高
3. 本地临时调试会被“收敛”覆盖
4. 目录结构变化（新建、删除、重命名）容易出现不一致

`workspace-sync` 的设计就是为了解决这些问题：

- 通过实时监听 + 快照重同步，保证最终一致
- 通过分块传输 + 断点续传，降低资源消耗并增强鲁棒性
- 通过 ACK / 重试机制，降低事件丢失概率
- 通过 `.nosync` 标记，让你可按目录暂停同步收敛

---

## 2. 功能总览

### 2.1 同步模式

- `send`：只发送（推荐生产）
- `receive`：只接收
- `both`：双向（可用，但冲突策略不建议复杂生产场景直接使用）

### 2.2 同步流程

- 启动时执行快照同步：
  - `snapshot_begin`
  - 文件按需 `upsert` 或 `snapshot_keep`
  - `snapshot_end`
- 运行中监听文件系统事件，实时下发变更
- 周期性 `--resync` 用于修复漏事件或手工改动

### 2.3 目录与文件事件

- 新建目录：`mkdir`
- 文件写入：`upsert_begin / upsert_chunk / upsert_end`
- 删除：`delete`
- 重命名/移动：`move`

### 2.4 可靠性补充

- 发送端事件通过队列串行化发送（单连接 ACK 顺序一致）
- 未完成的分块文件会以 `.workspace-sync.part` 暂存，支持超时清理（见 `--partial-ttl`）

以下是本次迭代完成并已落地到 `develop` 的关键能力：

### 3.1 可靠性增强：ACK + 重试

- 每个事件都有唯一 `event_id`
- 接收端处理后返回 `ack`
- 发送端支持：
  - `--ack-timeout`
  - `--max-retries`
- 可显著降低网络抖动场景下的丢事件风险

### 3.2 协议协商：hello / protocol / caps

- 建连后先发送 `hello`
- 双端交换协议版本与能力集（caps）
- 为后续协议演进与兼容留出空间

### 3.3 大文件分块传输

- 文件不再整块读入内存
- 使用 `--chunk-size` 控制分块大小
- 显著降低大文件同步时的内存峰值

### 3.4 断点续传（resume）

- 接收端在 `upsert_begin` 阶段可回传 `resume_from`
- 发送端从指定偏移继续发 chunk
- 网络中断后无需总是从 0 开始重传

### 3.5 并发发送队列

- 增加 worker 池并发处理发送任务
- `--send-workers`：发送 worker 数（单连接顺序发送，队列化执行）
- 在多小文件场景提升整体吞吐

### 3.6 resync 成本优化

- 先做 `size + mtime` 快速判断
- 仅在必要时计算 SHA256
- 减少无效 hash 计算和 IO 压力

### 3.7 可观测性（metrics）

- 周期输出关键指标：
  - sent / acked / retried / failed
  - bytes_sent / bytes_recv
  - snapshot sent/keep/prune
  - stop-skip / resume 次数
- 参数：`--metrics-interval`

### 3.8 `.nosync` 目录冻结（发送端 + 接收端）

在任意目录放置 `.nosync`，即可冻结该目录子树：

- 接收端：跳过事件应用与 prune（本地不会被收敛覆盖）
- 发送端：跳过该子树发送（减少无效流量）

恢复时删除 `.nosync` 即可。

> 注意：旧标记 `.stop` 已重命名为 `.nosync`。

---

## 4. 快速开始

### 4.1 接收端（示例）

```bash
workspace-sync-darwin-arm64 -r -d /path/to/recv -p 17077 --token "xxx"
```

### 4.2 发送端（示例）

```bash
workspace-sync-linux-amd64 -s -d /path/to/send --peer 1.2.3.4:17077 -p 17077 --token "xxx"
```

---

## 5. 参数说明

### 基础参数

- `--mode send|receive|both`（或 `-s` / `-r`）
- `--dir, -d`：同步目录
- `--peer`：发送目标（send/both 必填）
- `--listen, -l, -p`：监听地址/端口
- `--token, -t`：共享鉴权 token

### 同步节奏

- `--debounce`：事件去抖（默认 `400ms`）
- `--resync`：周期重同步（默认 `60s`，设为 `0` 可关闭）

### 可靠性与性能

- `--chunk-size`：分块大小（字节）
- `--ack-timeout`：ACK 等待超时
- `--max-retries`：单事件最大重试次数
- `--send-workers`：发送 worker 数（单连接顺序发送，队列化执行）
- `--enable-resume`：是否启用断点续传
- `--metrics-interval`：指标输出周期
- `--partial-ttl`：残留分块文件（.workspace-sync.part）清理时间（0 关闭清理）

### 排除规则

- `--exclude <glob>`（可重复）

---

## 6. `.nosync` 使用说明

冻结目录：

```bash
touch some/dir/.nosync
```

恢复目录同步：

```bash
rm some/dir/.nosync
```

推荐用于：

- 本地临时调试目录
- 大量短期实验数据目录
- 你不希望被自动收敛覆盖的子目录

---

## 7. 后台运行

- `start`：后台启动
- `status`：查看状态
- `stop`：停止进程

默认日志位置（Linux）：

- `~/.cache/workspace-sync/workspace-sync.log`
