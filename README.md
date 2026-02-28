# workspace-sync

一个基于 Go 的轻量级实时文件同步工具。用于把源端目录同步到目标端，并尽量保持两端一致。

## 当前版本功能（最新）

### 1) 同步机制

- 实时监听文件变化（创建/修改/删除）
- 启动时先做一次**全量快照同步**（initial snapshot）
- 默认开启**周期性全量重同步**（`--resync`，默认 `60s`）
  - 用于自动修复漏事件或目标端被手动改动/误删
- 快照结束后，接收端会清理“源端不存在”的文件并收敛目录

> 一句话：发送端是真源，接收端最终会收敛到发送端状态。

### 2) 目录操作增强

- 新目录创建会递归加入监控
- 目录变更后会补发子文件，降低“只 delete 不 upsert”的概率

### 3) 安全与传输

- 共享 token 鉴权
- 加密传输（AES-GCM）

### 4) 运行模式

- `send`：仅发送
- `receive`：仅接收
- `both`：双向（实验性；生产建议单向）

### 5) CLI 简化

- 已移除 Web 管理页与 Web 配置
- 纯命令行使用
- 支持后台管理：`start / status / stop`

---

## 你要的短命令（已支持）

### 接收端（mac）

```bash
workspace-sync-darwin-arm64 -r -d /Users/h1code2/48264/openclaw-workspace -p 17077 --token "h1code2"
```

### 发送端（linux）

```bash
workspace-sync-linux-amd64 -s -d ~/.openclaw/workspace-developer/ -p 17077 --peer 100.126.242.74:17077 --token "h1code2"
```

参数含义：
- `-r` / `-s`：receive / send
- `-d`：目录（`--dir`）
- `-p`：端口或监听地址（`--listen`，支持 `17077` 或 `:17077`）
- `-t` / `--token`：共享密钥
- `--peer`：发送目标（`ip[:port]`）
  - 只写 IP 时，会按 `-p` 自动补端口

---

## 关于“接收端删文件后不会恢复”

现在已修复：通过发送端的 `--resync` 周期重同步自动补回。

- `--resync` 设置在**发送端**
- 默认 `60s`
- `--resync 0` 可关闭

示例（每 20 秒重同步一次）：

```bash
workspace-sync-linux-amd64 -s -d ~/.openclaw/workspace-developer/ -p 17077 --peer 100.126.242.74:17077 --token "h1code2" --resync 20s
```

---

## 长参数兼容

- `--mode send|receive|both`
- `--dir /path`（短：`-d`）
- `--listen :7070`（短：`-p` 或 `-l`）
- `--peer host[:port]`
- `--token xxx`（短：`-t`）
- `--debounce 400ms`
- `--resync 60s`（0 禁用）
- `--exclude <pattern>`（可重复）
- `--pid-file /path/to.pid`

默认排除：
- `.git/*`
- `node_modules/*`
- `.DS_Store`

---

## 后台运行

```bash
workspace-sync -s -d /data/ws -p 17077 --peer 100.x.x.x --token "xxx" start
workspace-sync status
workspace-sync stop
```

默认 PID 文件：
- Linux: `~/.cache/workspace-sync/workspace-sync.pid`
- macOS: `~/Library/Caches/workspace-sync/workspace-sync.pid`

后台日志文件：
- Linux: `~/.cache/workspace-sync/workspace-sync.log`
- macOS: `~/Library/Caches/workspace-sync/workspace-sync.log`

日志大小控制：
- 默认最大 `10MB`
- 可用 `--log-max-bytes` 调整
- 也可通过环境变量 `WORKSPACE_SYNC_LOG_MAX_BYTES` 覆盖默认值

可用下面命令查看后台日志：

```bash
tail -f ~/.cache/workspace-sync/workspace-sync.log
```

---

## 一键构建（多架构）

项目内脚本：

```bash
./build-all.sh
```

会产出：
- `dist/workspace-sync-linux-amd64`
- `dist/workspace-sync-darwin-arm64`

构建参数包含 `-trimpath`。
