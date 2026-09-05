# Mihomo Node Manager

## 介绍

**TLDR: 一个部署在服务器上的程序，通过 systemd 管理（有安装脚本）。定期测试本机的 Mihomo API 和 CPA 反代的 API 的通达度，通过适时选择最佳的节点避免 IP 的风控**。

> [!note]
>
> 应用场景：服务器部署的 Astrbot 使用 CPA 反代的 API，但是经常出 400 错误，也就是 IP 被风控。通过部署此程序可适时调整。**通常需要至少一个节点是比较干净的**。

一个独立的 Mihomo 白名单节点管理服务。它定期通过 Mihomo REST API 测量节点延迟，在节点故障时快速转移，并在候选节点持续明显更优时平稳切换。

可以对 CPA（cli-proxy-api）反代的 Gemini API 做 **ping-pong 连通性探测**：Google 会按出口 IP 风控 Gemini API（`400 - User location is not supported`），延迟最低的节点未必能用。本服务会用一次极小的补全请求区分"能用"与"不能用"的节点，在保证可用的前提下选最快的。

服务只会选择 `config.toml` 中的 `allowed_nodes`，不会修改 Mihomo 配置或订阅文件。

后续 agent 维护前应先阅读 [`AGENTS.md`](AGENTS.md)，其中记录了安全不变量、真实 Mihomo provider API 差异和验证步骤；具体服务器的部署事实记录在不入库的 `AGENTS.local.md` 中。

## 选择策略（含 ping-pong 时）

每一轮（默认 60 秒）先做原有的 Google 204 延迟探测，再按以下优先级选节点：

1. **全部健康节点里，优先挑"通过 ping-pong"的**：在通过探测的节点里选 EWMA 延迟最低的；
2. **没有节点通过（或都未测过）**：在"不确定/未测"的健康节点里选最快的；切换到未测过的节点前会先做一次 ping-pong 测试，测出 400 就标记为脏并跳过，继续试下一个；
3. **所有健康节点都被判脏**：放弃 ping-pong 约束，直接选延迟最快的（保持连通性优先）。

补充规则：

- 当前节点变脏时，优先测试并切换到 `pingpong.safe_node`（需在 `allowed_nodes` 内，默认为空 = 不启用该偏好），失败再按延迟顺序扫描；手动指定的节点变脏同样会提前退出手动模式。
- 脏节点标记保留 `fail_ttl_seconds`（默认 30 分钟），过期后重新视为"未测"，允许再次被测试（例如服务商更换了出口 IP）。
- 只有"脏判定规则"命中的响应才判脏（默认是 Google 的位置风控 400）；`503 auth_unavailable`（CPA 刷新 OAuth 凭证）、429、超时等一律视为**不确定**，不会导致切换。
- ping-pong 需要验证"未测过"的节点时会短暂把策略组切到该节点发一次请求，测完立即落到最终选择；切换/测试后会关闭该策略组的既有连接，强制客户端从新节点重新拨号（`close_conns_on_switch`）。
- 当前节点每隔 `refresh_interval_seconds`（默认 5 分钟）静默复测一次，无需切换组。

## 配置 .env（CPA 探测的凭据）

CPA 的地址、API Key 和模型名不写进 `config.toml`，而是从环境变量读取，可由 `.env` 文件注入。模板见 [`.env.example`](.env.example)。

**本地开发**（仓库根目录）：

```sh
cp .env.example .env
# 编辑 .env，填写：
#   CPA_BASE_URL  例如 http://127.0.0.1:<端口> （写到 /v1 这一级即可，也接受完整 /v1/chat/completions）
#   CPA_API_KEY   CPA 未开鉴权就留空
#   CPA_MODEL     必须与你的 CPA 实际支持的模型名完全一致
```

**服务器**：

```sh
sudo cp /etc/mihomo-node-manager/.env.example /etc/mihomo-node-manager/.env
sudo $EDITOR /etc/mihomo-node-manager/.env        # 同样填 CPA_BASE_URL / CPA_API_KEY / CPA_MODEL
sudo chown root:mihomo-node-manager /etc/mihomo-node-manager/.env
sudo chmod 640 /etc/mihomo-node-manager/.env
sudo systemctl restart mihomo-node-manager
```

服务端的 `.env` 之所以放在 `/etc/mihomo-node-manager/`，是因为 systemd unit 把 `WorkingDirectory` 指向了该目录，配置默认值 `pingpong.env_file = ".env"` 就会解析到这里。也可以不改文件，直接在 unit 里用 `Environment=`/`EnvironmentFile=` 注入 `CPA_*` 变量（进程环境变量优先于 `.env` 文件）。

`CPA_BASE_URL` 与 `CPA_MODEL` 都非空时探测才会启用；两者只填其一会在启动时直接报错。全部留空 = 功能关闭，行为与旧版本一致。`pingpong.*` 的其余参数（复测间隔、脏标记 TTL、超时、safe_node 等）在 `config/config.example.toml` 的 `[pingpong]` 段。

## 构建与检查

```sh
make race vet build
./outputs/mihomo-node-manager --config config/config.example.toml --check-config
./outputs/mihomo-node-manager --config config/config.example.toml --once --dry-run
```

`--once --dry-run` 会连接 Mihomo 并探测节点，不会发出切换请求，也不会写状态文件。若已配置 CPA，它还会对**当前节点**发一次 ping-pong 请求（这是探测，不是切换；会给 CPA 带来一次极小的请求量），并对"需要先测试才能切换"的候选节点只输出计划。

## 本机 API

默认监听 `127.0.0.1:9123`：

```sh
curl -sS http://127.0.0.1:9123/healthz
curl -sS http://127.0.0.1:9123/v1/status
curl -sS http://127.0.0.1:9123/v1/nodes

curl -sS -X POST http://127.0.0.1:9123/v1/switch \
  -H 'Content-Type: application/json' \
  -d '{"node":"node-02","force":false}'

curl -sS -X POST http://127.0.0.1:9123/v1/auto

# ping-pong 探测：空 body = 测当前出口节点
curl -sS -X POST http://127.0.0.1:9123/v1/pingpong
# 指定节点：会短暂切过去测一下再恢复
curl -sS -X POST http://127.0.0.1:9123/v1/pingpong -d '{"node":"node-02"}'
# 全量扫描所有白名单节点，最后落在策略会选的节点上（可能需要一两分钟）
curl -sS -X POST http://127.0.0.1:9123/v1/pingpong -d '{"force":true}'
```

普通手动切换会先探测目标节点，探测失败返回 HTTP 409；启用 ping-pong 后，手动切换到"脏"或未验证的节点会先做 ping-pong 测试，失败同样返回 409（`pingpong_failed`），并自动恢复原选择。显式传入 `"force":true` 可以同时跳过这两项检查（仍受白名单约束）。手动选择默认保持 30 分钟，节点连续失败两次或被 ping-pong 判脏时会提前故障转移。

`/v1/nodes` 的每个节点现在带有 `pingpong` 字段（`pass` / `dirty` / `inconclusive` / `unknown`、最近测试时间、往返延迟与详情），可用于确认哪些节点被 Google 风控。

## 服务器部署（源码编译）

克隆源码并构建（需要 Go ≥ 1.24）：

```sh
git clone <本仓库> && cd mihomo-node-manager
make build
sudo ./deploy/install.sh
```

安装脚本会把 `outputs/mihomo-node-manager` 复制为 root 所有的 `/usr/local/bin/mihomo-node-manager`，并安装带沙箱加固的 systemd unit、专用服务用户与默认配置。脚本**从不覆盖**已有的 `/etc/mihomo-node-manager/config.toml` 和 `.env`（新配置写为 `config.toml.new`）。首次部署时注意把 `config.toml.new` 里的 `[pingpong]` 段合并进现有配置，然后按上一节创建 `/etc/mihomo-node-manager/.env`。

之后的升级：

```sh
git pull && make build
sudo ./deploy/install.sh          # 重新安装二进制副本与 unit
sudo systemctl restart mihomo-node-manager
```

查看运行状态和日志：

```sh
systemctl status mihomo-node-manager
journalctl -u mihomo-node-manager -f
curl -sS http://127.0.0.1:9123/v1/status
```

## 停用与回滚

停止服务会保留最后固定节点。需要恢复 Mihomo 原生 URLTest 行为时执行：

```sh
sudo systemctl disable --now mihomo-node-manager
sudo /usr/local/bin/mihomo-node-manager \
  --config /etc/mihomo-node-manager/config.toml \
  --clear-fixed
```

这只通过 Mihomo API 清除 `PROXY` 组的 fixed 状态，不修改 Mihomo 配置文件。
