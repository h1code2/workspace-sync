# workspace-sync

一个基于 Go 的轻量级实时文件同步工具。目标是把一个目录稳定地同步到另一台机器，并尽量保持两端文件一致。

## 你要的短命令（已支持）

### 接收端

```bash
workspace-sync -r -p /xxx/path -p :8088 --token "xxxx"
```

含义：
- `-r` = receive
- 第一个 `-p` = 本地目录路径（等价 `--dir`）
- 第二个 `-p` = 监听地址/端口（等价 `--listen`）

### 发送端

```bash
workspace-sync -s -p /xxx/path -p :8088 --peer "100.x.x.x" --token "xxxx"
```

含义：
- `-s` = send
- 第一个 `-p` = 本地目录路径（等价 `--dir`）
- 第二个 `-p` = 本地端口配置（会用于补全 `--peer` 端口）
- `--peer "100.x.x.x"` 会自动补全为 `100.x.x.x:8088`

> 也支持你直接写全：`--peer "100.x.x.x:8088"`

---

## 兼容的长参数

- `--mode send|receive|both`
- `--dir /path`
- `--listen :7070`
- `--peer host[:port]`
- `--token xxx`
- `--debounce 400ms`
- `--exclude <pattern>`（可重复）

默认排除：`.git/*`, `node_modules/*`, `.DS_Store`

---

## 后台运行

```bash
# 后台启动（参数同上）
workspace-sync -s -p /data/ws -p :8088 --peer 100.x.x.x --token "xxx" start

# 查看状态
workspace-sync status

# 停止
workspace-sync stop
```
