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

---

## 所有参数说明（完整）

### 模式与目标

- `--mode send|receive|both`
  - 运行模式。默认：`send`
  - **发送端：`send`；接收端：`receive`**
- `-r`
  - `--mode=receive` 的快捷方式（**接收端使用**）
- `-s`
  - `--mode=send` 的快捷方式（**发送端使用**）
- `--peer <host[:port]>`
  - 发送目标地址（`send/both` 必填，**发送端使用**）
  - 若只写主机（不带端口），会自动使用 `--listen/-p` 的端口补齐

### 路径与监听

- `--dir <path>` / `-d <path>`
  - 同步目录
- `--listen <addr>` / `-l <addr>` / `-p <addr-or-port>`
  - 接收监听地址，默认 `:7070`
  - 支持 `17077`（会自动变成 `:17077`）或 `:17077` 或 `0.0.0.0:17077`

### 鉴权与同步节奏

- `--token <string>` / `-t <string>`
  - 共享密钥（必填）
- `--debounce <duration>`
  - 文件事件去抖，默认 `400ms`
  - 示例：`200ms` / `1s`
- `--resync <duration>`
  - 周期性全量重同步（发送端生效）
  - 默认 `60s`，设 `0` 关闭

### 过滤规则

- `--exclude <glob>`（可重复）
  - 排除规则，可多次传入
  - 默认排除：`.git/*`, `node_modules/*`, `.DS_Store`
  - **建议配置端：发送端（-s）**
    - 发送端决定“哪些文件会被发出”，这是主控制点
  - 接收端也可配置同样规则作为二次保险，但不是主要控制面

### 后台管理

- 子命令：`start | status | stop`
  - `start`：按当前参数后台启动
  - `status`：查看 PID 是否在运行
  - `stop`：停止后台进程
- `--pid-file <path>`
  - PID 文件路径
  - 默认：
    - Linux：`~/.cache/workspace-sync/workspace-sync.pid`
    - macOS：`~/Library/Caches/workspace-sync/workspace-sync.pid`

### 日志相关

- `--log-file <path>`
  - 后台日志文件路径
  - 默认：
    - Linux：`~/.cache/workspace-sync/workspace-sync.log`
    - macOS：`~/Library/Caches/workspace-sync/workspace-sync.log`
- `--log-max-bytes <n>`
  - 日志最大保留字节数，默认 `10485760`（10MB）
  - 超过后自动裁剪（保留最新日志）
- 环境变量：`WORKSPACE_SYNC_LOG_MAX_BYTES`
  - 可覆盖默认日志上限（10MB）

### 内部参数（无需手动设置）

- `--background-child`
  - 仅内部后台拉起时使用，正常不用传

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

## 后台运行示例

```bash
workspace-sync -s -d /data/ws -p 17077 --peer 100.x.x.x --token "xxx" start
workspace-sync status
workspace-sync stop
```

查看后台日志：

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
