# OpenClaw Dameon

`dameon` 是 Ubuntu 设备侧守护进程工程目录，负责承接 `docs/daemon-dev-plan.md` 中的 MVP 目标：

* 连接云端 Netty WebSocket 网关并执行 `auth_req` / `ping`
* 接收 `sys_config` 并将配置落地到 `~/.openclaw/.env` 与 `~/.openclaw/openclaw.json`
* 通过可配置命令执行 OpenClaw 重启和健康检查
* 执行受控 `remote_cmd` 并回传 `remote_cmd_result`
* 为后续 OpenClaw Gateway 官方协议适配预留 `gateway-adapter`

当前默认云端网关地址设为 `ws://43.156.161.7:18080/ws/device`，对应 [application.yml](/Users/lvfushun/IdeaProjects/openclaw/edge-gateway/src/main/resources/application.yml#L14) 里的 Netty 端口与路径。

## 当前实现范围

已完成：

* Go 工程骨架与主入口
* 云端 WebSocket 客户端、断线重连、心跳
* 协议模型与消息路由
* 配置版本幂等、本地状态文件、原子写入
* 远程命令白名单、超时和输出截断
* 云端网关地址的默认值、环境变量覆盖和 `sys_config` 持久化更新

待补充：

* `chat_msg -> OpenClaw Gateway Protocol` 的正式适配
* 更完整的 `pm2`/`systemd` 运行监督与 crash restart 协调
* 针对官方 `auth-profiles.json` 的 provider 凭证落地

## 本地启动

1. 复制示例配置：

```bash
cp config/daemon.example.json config/daemon.json
```

2. 启动：

```bash
go run ./cmd/openclaw-daemon
```

也可以通过环境变量指定配置路径：

```bash
OPENCLAW_DAEMON_CONFIG=/path/to/daemon.json go run ./cmd/openclaw-daemon
```

云端网关地址支持环境变量覆盖，适用于 `root` 或其他运行用户：

```bash
OPENCLAW_CLOUD_WS_URL=ws://43.156.161.7:18080/ws/device go run ./cmd/openclaw-daemon
```

也可以拆分为 host/port/path：

```bash
OPENCLAW_CLOUD_HOST=43.156.161.7 \
OPENCLAW_CLOUD_PORT=18080 \
OPENCLAW_CLOUD_WS_PATH=/ws/device \
go run ./cmd/openclaw-daemon
```

如果后端通过 `sys_config` 下发 `CLOUD_WS_URL` 或 `EDGE_GATEWAY_WS_URL`，Daemon 会把新地址写回 `daemon.json`，之后重连时自动使用新地址。

## 交叉编译

当前依赖只有 `github.com/gorilla/websocket`，属于纯 Go 包，不依赖 CGO 或系统原生库，适合直接交叉编译。

Linux AMD64:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/openclaw-daemon-linux-amd64 ./cmd/openclaw-daemon
```

Linux ARM64:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o build/openclaw-daemon-linux-arm64 ./cmd/openclaw-daemon
```
