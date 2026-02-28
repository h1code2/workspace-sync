# workspace-sync

一个基于 Go 的轻量级实时文件同步工具。目标是把一个目录稳定地同步到另一台机器，并尽量保持两端文件一致。

## 你这次要的命令格式（已支持）

### 接收端（mac）

```bash
workspace-sync-darwin-arm64 -r -d /Users/h1code2/48264/openclaw-workspace -p 17077 --token "h1code2"
```

### 发送端（linux）

```bash
workspace-sync-linux-amd64 -s -d ~/.openclaw/workspace-developer/ -p 17077 --peer 100.126.242.74:17077 --token "h1code2"
```

说明：
- `-r` / `-s`：receive / send
- `-d`：目录（`--dir`）
- `-p`：端口或监听地址（`--listen`，支持 `17077` 或 `:17077`）
- `--token`（也支持 `-t`）
- `--peer`：发送端目标（`ip:port`，只写 ip 也会按 `-p` 自动补端口）

---

## 为什么“接收端手动删文件后不会自动恢复”？

已修复：现在默认开启**周期性全量重同步**，会自动把接收端误删文件补回来。

- 新参数：`--resync`（默认 `60s`）
- 设为 `0` 可关闭周期重同步

示例：每 20 秒收敛一次

```bash
workspace-sync-linux-amd64 -s -d ~/.openclaw/workspace-developer/ -p 17077 --peer 100.126.242.74:17077 --token "h1code2" --resync 20s
```

---

## 兼容长参数

- `--mode send|receive|both`
- `--dir /path`
- `--listen :7070`
- `--peer host[:port]`
- `--token xxx` / `-t xxx`
- `--debounce 400ms`
- `--resync 60s`（0 禁用）
- `--exclude <pattern>`（可重复）

默认排除：`.git/*`, `node_modules/*`, `.DS_Store`

---

## 后台运行

```bash
workspace-sync -s -d /data/ws -p 17077 --peer 100.x.x.x --token "xxx" start
workspace-sync status
workspace-sync stop
```
