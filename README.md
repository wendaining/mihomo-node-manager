# Mihomo Node Manager

一个独立的 Mihomo 白名单节点管理服务。它定期通过 Mihomo REST API 测量节点延迟，在节点故障时快速转移，并在候选节点持续明显更优时平稳切换。

服务只会选择 `config.toml` 中的 `allowed_nodes`，不会修改 Mihomo 配置或订阅文件。

后续 agent 维护前应先阅读 [`AGENTS.md`](AGENTS.md)，其中记录了安全不变量、服务器事实、真实 Mihomo provider API 差异和验证步骤。

## 构建与检查

```sh
make race vet build
./outputs/mihomo-node-manager --config config/config.toml --check-config
./outputs/mihomo-node-manager --config config/config.toml --once --dry-run
```

`--once --dry-run` 会连接 Mihomo 并探测节点，但不会发出切换请求，也不会写入状态文件。

## 本机 API

默认监听 `127.0.0.1:9123`：

```sh
curl -sS http://127.0.0.1:9123/healthz
curl -sS http://127.0.0.1:9123/v1/status
curl -sS http://127.0.0.1:9123/v1/nodes

curl -sS -X POST http://127.0.0.1:9123/v1/switch \
  -H 'Content-Type: application/json' \
  -d '{"node":"🇺🇸 美国 02","force":false}'

curl -sS -X POST http://127.0.0.1:9123/v1/auto
```

普通手动切换会先探测目标节点；探测失败时返回 HTTP 409。显式传入 `"force":true` 可以强制切换到仍存在于白名单和 Mihomo 策略组中的节点。手动选择默认保持 30 分钟，节点连续失败两次时会提前故障转移。

## systemd 部署

部署包包含二进制、最终配置、unit 和安装脚本。上传后运行：

```sh
sudo ./install.sh
```

查看运行状态和日志：

```sh
systemctl status mihomo-node-manager
journalctl -u mihomo-node-manager -f
curl -sS http://127.0.0.1:9123/v1/status
```

安装脚本不会覆盖已有配置；升级时新配置写为 `/etc/mihomo-node-manager/config.toml.new`。

## 停用与回滚

停止服务会保留最后固定节点。需要恢复 Mihomo 原生 URLTest 行为时执行：

```sh
sudo systemctl disable --now mihomo-node-manager
sudo /usr/local/bin/mihomo-node-manager \
  --config /etc/mihomo-node-manager/config.toml \
  --clear-fixed
```

这只通过 Mihomo API 清除 `PROXY` 组的 fixed 状态，不修改 Mihomo 配置文件。
