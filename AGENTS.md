# Agent Ground Truth

本文是维护 `mihomo-node-manager` 时的最小事实集。先读本文，再改代码或服务器；动态状态仍应通过只读命令重新确认。

## 目标与硬约束

- 这是独立的 Go/systemd 基础服务，不依赖 AstrBot 或其他 agent。
- 只通过 Mihomo REST API 探测和切换，不修改 `/etc/mihomo`、订阅或 `mihomo.service`。
- 任何自动或手动切换都必须严格限制在 `allowed_nodes`；即使 `force=true` 也不能越过白名单或选择不在 `PROXY` 组中的节点。
- 停止服务时保留最后的 fixed 选择。只有显式执行 `--clear-fixed` 才恢复 Mihomo 原生 URLTest。
- API 无鉴权，因此配置校验强制其监听 loopback；不要在没有新增鉴权的情况下放宽此限制。

## 当前部署事实

- 目标 SSH 主机：`tencent-lighthouse`，Ubuntu amd64，systemd 255。
- Mihomo：Meta 1.19.30，服务名 `mihomo.service`，API 为 `http://127.0.0.1:9090`，策略组为 URLTest 类型的 `PROXY`。
- Mihomo API 当前没有 secret；代码已支持从 `mihomo.secret_file` 读取 Bearer Token。
- 管理 API：`127.0.0.1:9123`；状态：`/healthz`、`/v1/status`、`/v1/nodes`；操作：`POST /v1/switch`、`POST /v1/auto`。
- 系统安装路径：二进制 `/usr/local/bin/mihomo-node-manager`，配置 `/etc/mihomo-node-manager/config.toml`，状态 `/var/lib/mihomo-node-manager/state.json`。
- 0.1.0 已于 2026-09-05 安装、启用并完成真实探测、写接口与重启恢复测试；安装包保留在远端 `/home/wendaining/mihomo-node-manager-deploy-0.1.0`。动态状态仍应先用 `systemctl is-active mihomo-node-manager` 确认。
- 状态文件已验证为 `mihomo-node-manager:mihomo-node-manager` 所有、权限 `0600`；管理 API 与 Mihomo API 均仅监听 `127.0.0.1`。

当前白名单的精确名称（大小写和 emoji 都是接口标识的一部分）：

```text
🇯🇵 日本 01
🇯🇵 日本 02
🇺🇸 美国 01
🇺🇸 美国 02
🇺🇸 美国 03
US-Reality-device-1
```

## Mihomo API 的关键事实

- 这些节点来自 proxy provider。它们虽然出现在 `GET /proxies/PROXY` 的 `all` 中，但这台服务器对 `/proxies/{node}/delay` 返回 404。
- 正确流程是读取 `GET /providers/proxies`，解析节点所属 provider，再调用：

  ```text
  GET /providers/proxies/{provider}/{node}/healthcheck?url=...&timeout=5000&expected=204
  ```

- 切换使用 `PUT /proxies/PROXY` 和 `{"name":"节点名"}`；成功后必须再次读取组并核对 `now` 和 `fixed`。
- 不要使用 `/group/PROXY/delay`：该接口会清除自动策略组的 fixed 选择，破坏本服务的控制权。
- provider 中出现重名节点时客户端会拒绝继续，因为无法安全确定目标。

## 策略真值

配置默认值以 `config/config.toml` 为准：

- 每 60 秒探测，Google 204，超时 5 秒，并发 4，保留最近 10 次结果。
- 延迟使用 alpha `0.35` 的 EWMA；首次成功即可参与选择。
- 当前节点连续失败 2 次立即故障转移；失效节点连续成功 2 次后恢复候选资格。
- 优化切换需要同时满足：驻留至少 600 秒、同一候选连续优胜 3 轮、延迟降低至少 20% 或 100ms。
- 手动选择默认持续 1800 秒；仍持续探测，手动节点连续失败 2 次会提前退出手动模式并转移。
- 全部候选失败时保留最后的白名单节点，不回退到白名单外节点、DIRECT 或 REJECT。
- Mihomo 重启或配置重载造成 `now`/`fixed` 漂移时，下一轮会重新固定期望节点。
- `--once --dry-run` 可以探测并输出决策，但绝不 PUT 或写状态文件。

## 代码地图

- `cmd/mihomo-node-manager/main.go`：CLI、日志、信号处理和服务生命周期。
- `internal/config`：TOML 默认值和严格校验；未知键会报错。
- `internal/mihomo`：HTTP 客户端、provider 解析、探测、选择及选择后核验。
- `internal/manager`：并发探测、状态机、EWMA、故障转移、手动模式和 API 快照。
- `internal/state`：版本化 JSON 状态和 fsync + rename 原子写入；损坏状态会告警并冷启动。
- `internal/api`：loopback JSON API、请求校验和稳定错误格式。
- `deploy`：systemd unit 与幂等安装脚本；已有配置不会被覆盖，新配置写为 `.new`。

## 修改与验证

修改策略或 API 前必须保持上述硬约束，并运行：

```sh
gofmt -w cmd internal
go mod verify
go vet ./...
go test -race ./...
make build
./outputs/mihomo-node-manager --config config/config.toml --check-config
```

接触真实服务器时，先做无副作用验证：

```sh
./mihomo-node-manager --config ./config.toml --once --dry-run
```

部署后至少核对：

```sh
systemctl --no-pager --full status mihomo-node-manager
journalctl -u mihomo-node-manager -n 100 --no-pager
curl -sS http://127.0.0.1:9123/healthz
curl -sS http://127.0.0.1:9123/v1/status
curl -sS http://127.0.0.1:9123/v1/nodes
```

新增或重命名节点时，必须从真实的 `/providers/proxies` 与 `/proxies/PROXY` 同时确认精确名称；不要根据订阅文本或人工显示名猜测。
