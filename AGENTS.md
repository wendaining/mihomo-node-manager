# Agent Ground Truth

本文是维护 `mihomo-node-manager` 时的最小事实集。先读本文，再改代码或服务器；动态状态仍应通过只读命令重新确认。

## 目标与硬约束

- 这是独立的 Go/systemd 基础服务，不依赖 AstrBot 或其他 agent。
- 只通过 Mihomo REST API 探测和切换，不修改 `/etc/mihomo`、订阅或 `mihomo.service`。
- 任何自动或手动切换都必须严格限制在 `allowed_nodes`；即使 `force=true` 也不能越过白名单或选择不在 `PROXY` 组中的节点。
- ping-pong 验证未测节点时会短暂把 `PROXY` 组切到该候选节点，测试后立刻落到最终选择；绝不切换到白名单外的节点去测试。
- 停止服务时保留最后的 fixed 选择。只有显式执行 `--clear-fixed` 才恢复 Mihomo 原生 URLTest。
- API 无鉴权，因此配置校验强制其监听 loopback；不要在没有新增鉴权的情况下放宽此限制。
- CPA 凭据只存在于环境变量 / `/etc/mihomo-node-manager/.env`（权限 0640，root:mihomo-node-manager），不写入 config.toml、不提交 git。

## 当前部署事实

- 目标 SSH 主机：`tencent-lighthouse`，Ubuntu amd64，systemd 255。
- Mihomo：Meta 1.19.30，服务名 `mihomo.service`，API 为 `http://127.0.0.1:9090`，策略组为 URLTest 类型的 `PROXY`。
- Mihomo API 当前没有 secret；代码已支持从 `mihomo.secret_file` 读取 Bearer Token。
- 管理 API：`127.0.0.1:9123`；状态：`/healthz`、`/v1/status`、`/v1/nodes`；操作：`POST /v1/switch`、`POST /v1/auto`、`POST /v1/pingpong`。
- 系统安装路径：**二进制不落盘到系统目录**——systemd 直接执行 `/home/wendaining/mihomo-node-manager-src/outputs/mihomo-node-manager`（unit `ProtectHome=read-only` + `/home/wendaining` 已 `chmod o+x`，均为一次性设置）；配置 `/etc/mihomo-node-manager/config.toml`，状态 `/var/lib/mihomo-node-manager/state.json`，CPA 凭据 `/etc/mihomo-node-manager/.env`。`/usr/local/bin` 下的旧副本与 `/home/wendaining/mihomo-node-manager-deploy-0.1.0` 旧部署包已于 2026-09-05 删除，不要重建引用。
- 0.1.0 部署包方式已废弃。当前部署事实：服务器源码位于 `/home/wendaining/mihomo-node-manager-src`（克隆自 GitHub，Go 1.27.1 在 `/usr/local/go/bin`，构建用 `GOPROXY=https://goproxy.cn,direct`）。升级流程：`git pull && make build && sudo systemctl restart mihomo-node-manager`；只有 unit/配置模板变化时才需要重跑 `sudo deploy/install.sh`（脚本只支持源码仓库布局，不覆盖已有 config.toml 与 .env）。
- systemd unit 的 `WorkingDirectory=/etc/mihomo-node-manager`，因此配置默认值 `pingpong.env_file = ".env"` 解析为 `/etc/mihomo-node-manager/.env`；本地开发则解析为仓库根目录的 `.env`。进程环境变量优先于 `.env` 文件。
- 状态文件为 `mihomo-node-manager:mihomo-node-manager` 所有、权限 `0600`，schema version 2（v1 文件可读，pingpong 字段从"未测"冷启动）；管理 API 与 Mihomo API 均仅监听 `127.0.0.1`。

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
- Mihomo 切换组后，既有连接仍走原节点的出口。代码通过 `GET /connections` + `DELETE /connections/{id}` 关闭链路中包含 `PROXY` 的连接，强制客户端（含 CPA）立刻从新节点重新拨号；这也是 ping-pong 结果不被旧连接污染的前提。由 `pingpong.close_conns_on_switch` 控制。
- 不要使用 `/group/PROXY/delay`：该接口会清除自动策略组的 fixed 选择，破坏本服务的控制权。
- provider 中出现重名节点时客户端会拒绝继续，因为无法安全确定目标。

## 策略真值

配置默认值以 `config/config.example.toml` 为准：

- 每 60 秒探测，Google 204，超时 5 秒，并发 4，保留最近 10 次结果。
- 延迟使用 alpha `0.35` 的 EWMA；首次成功即可参与选择。
- 当前节点连续失败 2 次立即故障转移；失效节点连续成功 2 次后恢复候选资格。
- 优化切换需要同时满足：驻留至少 600 秒、同一候选连续优胜 3 轮、延迟降低至少 20% 或 100ms。
- 手动选择默认持续 1800 秒；仍持续探测，手动节点连续失败 2 次或被 ping-pong 判脏会提前退出手动模式并转移。
- 全部候选失败时保留最后的白名单节点，不回退到白名单外节点、DIRECT 或 REJECT。
- Mihomo 重启或配置重载造成 `now`/`fixed` 漂移时，下一轮会重新固定期望节点。
- `--once --dry-run` 可以探测并输出决策，但绝不 PUT 或写状态文件。

ping-pong（CPA Gemini 探测，`CPA_BASE_URL` 与 `CPA_MODEL` 都配置后才启用，见 `.env.example`）：

- 判据：只有 CPA 返回 HTTP 400 且 body 含 "User location is not supported" / `FAILED_PRECONDITION` 才判节点"脏"；503 `auth_unavailable`（CPA 刷 OAuth）、429、超时等一律"不确定"，不影响选择。
- 选择优先级：健康且通过 ping-pong 的节点里选 EWMA 最快；否则在"非脏"节点里选最快（切换前先测试未验证的候选，测出脏就跳过换下一个）；全部脏时放弃约束、按纯延迟选最快。
- 当前节点变脏时先试 `pingpong.safe_node`（默认 `US-Reality-device-1`，必须在 allowed_nodes 内），再按延迟顺序扫描。
- 脏标记保留 `fail_ttl_seconds`（默认 1800 秒）后回到"未测"；当前节点每 `refresh_interval_seconds`（默认 300 秒）静默复测一次，无需切换组。
- ping-pong 结果持久化在状态文件中（`pingpong_*` 字段），重启不丢。

## 代码地图

- `cmd/mihomo-node-manager/main.go`：CLI、日志、信号处理和服务生命周期。
- `internal/config`：TOML 默认值和严格校验；未知键会报错。`[pingpong]` 段在此定义；`loadEnvironment` 先经 `internal/dotenv` 加载 env_file，再读 `CPA_*` 进程环境变量。
- `internal/dotenv`：极简 KEY=VALUE 解析器，不覆盖已存在的环境变量；文件缺失不报错，格式错误硬报错。
- `internal/pingpong`：CPA 补全请求与结果分类（pass / dirty / inconclusive）；`NormalizeEndpoint` 接受 base URL、`/v1` 或完整 endpoint 三种写法。
- `internal/mihomo`：HTTP 客户端、provider 解析、探测、选择及选择后核验；`CloseGroupConnections` 负责按链路关闭组内连接。
- `internal/manager`：并发探测、状态机、EWMA、故障转移、手动模式、ping-pong 门控（test-then-switch、safe_node、手动切换预检）和 API 快照。
- `internal/state`：版本化 JSON 状态（v2）和 fsync + rename 原子写入；损坏状态会告警并冷启动。
- `internal/api`：loopback JSON API、请求校验和稳定错误格式；`POST /v1/pingpong` 支持 `{"node":...}`（测单个并恢复原选择）与 `{"force":true}`（全量扫描）。
- `deploy`：systemd unit（含 `WorkingDirectory`）与幂等安装脚本；已有配置和 `.env` 不会被覆盖，新配置写为 `.new`。

## 修改与验证

修改策略或 API 前必须保持上述硬约束，并运行：

```sh
gofmt -w cmd internal
go mod verify
go vet ./...
go test -race ./...
make build
./outputs/mihomo-node-manager --config config/config.example.toml --check-config
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
curl -sS -X POST http://127.0.0.1:9123/v1/pingpong
```

新增或重命名节点时，必须从真实的 `/providers/proxies` 与 `/proxies/PROXY` 同时确认精确名称；不要根据订阅文本或人工显示名猜测。白名单即 ping-pong 的探测范围，新增节点无需额外配置；但 `safe_node` 引用的名字必须在白名单内。
