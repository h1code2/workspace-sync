# workspace-sync

一个基于 Go 的轻量级实时文件同步工具。目标是把一个目录稳定地同步到另一台机器，并尽量保持两端文件一致。

## 功能

- 实时监听文件变化（创建 / 修改 / 删除）
- 启动后先做一次全量快照同步（把现有文件先对齐）
- 支持模式：
  - `send`：仅发送本机变更
  - `receive`：仅接收对端变更
  - `both`：双向（实验性，生产建议单向）
- 支持后台运行：`start / status / stop`
- 传输鉴权与加密（基于共享 token）

> 当前同步是“文件级 upsert/delete”语义，不是块级增量。

---

## 构建

```bash
cd workspace-sync
go mod tidy
go build -o workspace-sync ./cmd/workspace-sync
```

---

## 快速开始（服务器 -> 本地 Mac）

### 1) 本地 Mac（接收端）

```bash
./workspace-sync \
  --mode=receive \
  --dir=/path/to/local/workspace \
  --listen=:7070 \
  --token='replace-with-strong-secret'
```

### 2) 服务器（发送端）

```bash
./workspace-sync \
  --mode=send \
  --dir=/path/to/server/workspace \
  --peer=<mac-ip>:7070 \
  --token='replace-with-strong-secret'
```

---

## 后台运行

可直接使用同一套参数加子命令：

```bash
# 后台启动（按当前参数启动）
./workspace-sync --mode=send --dir=/data/ws --peer=100.x.x.x:7070 --token='xxx' start

# 查看状态
./workspace-sync status

# 停止
./workspace-sync stop
```

默认 PID 文件：
- Linux: `~/.cache/workspace-sync/workspace-sync.pid`
- macOS: `~/Library/Caches/workspace-sync/workspace-sync.pid`

可通过 `--pid-file` 自定义。

---

## 参数

- `--mode`：`send | receive | both`
- `--dir`：同步目录
- `--listen`：接收端监听地址（默认 `:7070`）
- `--peer`：发送端对端地址
- `--token`：两端一致的共享密钥
- `--debounce`：文件事件去抖（默认 `400ms`）
- `--exclude`：排除规则（可重复）
- `--pid-file`：后台管理用 PID 文件路径

默认排除：
- `.git/*`
- `node_modules/*`
- `.DS_Store`

---

## 注意事项

- 建议在受信网络（Tailscale / WireGuard / 局域网）中使用
- 若发送端重启，会触发一次全量快照同步并收敛一致
- 生产场景建议单向同步，明确“源端”和“目标端”
