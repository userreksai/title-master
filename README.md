# Title Master

Go 编写的域名标题监控主控。读取现有 SEO 后端 `/api/v1/domains` 的启用域名，交给多个 8003 agent 检测，只在已确认的标题/异常状态变化时发送 webhook。

## 服务器部署

要求 Linux、运行中的 systemd、Git、Go 1.23+、基本 coreutils；下载脚本需要 curl。以 root 执行部署脚本。默认路径为 **`/user/local/title_master`**。

```sh
mkdir -p /user/local/title_master
cd /user/local/title_master
curl -fsSL https://raw.githubusercontent.com/userreksai/title-master/main/deploy.sh -o deploy.sh
sh deploy.sh
```

首次执行会下载默认分支最新代码、编译 `title-master`，生成 `master.json`，创建服务用户 `title-master` 和 `/etc/systemd/system/title-master.service`。默认配置含占位 token，首次只安装，不启动未配置的服务。

编辑 `/user/local/title_master/master.json`：

- `seo.base_url`：现有 SEO 后端地址，默认 `http://127.0.0.1:10001`。
- `seo.token`：SEO 服务的 `API_TOKEN`，无鉴权才留空。
- `agents`：三台机器的 `name`、`http://IP:8003` 和对应 token；可增删节点。
- `webhooks.domain_urls`：业务群 webhook 地址数组，可配置多个。
- `webhooks.machine_urls`：机器故障群 webhook 地址数组，可配置多个。
- `webhooks.format`：默认 `feishu`，也支持 `wecom`、`generic`。

完成配置后再次执行：

```sh
cd /user/local/title_master
sh deploy.sh
systemctl status title-master --no-pager
journalctl -u title-master -f
```

配置检查通过后，脚本会启动服务并设置开机自启。master 不需要新增监听端口。

## 更新

```sh
cd /user/local/title_master
curl -fsSL https://raw.githubusercontent.com/userreksai/title-master/main/deploy.sh -o deploy.sh
sh deploy.sh
```

每次执行都重新下载最新代码。`master.json`、状态基线和未发送告警不会被覆盖。源码在临时目录编译，结束后清理。下载/编译失败或已有配置无效时不替换旧程序；服务重启失败时恢复旧程序和 systemd 文件。另保留 `title-master.previous`，`title-master.version` 记录成功部署的 Git commit。

服务以独立用户运行；配置文件是 `root:title-master` 的 0640 权限，状态目录是服务用户的 0700 权限。不要删除状态文件。首次安装没有旧程序时无法回滚到旧版本，但配置和状态仍保留。

## 参数

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `interval_seconds` | 600 | 每 10 分钟一轮，15 分钟改 900 |
| `per_agent_concurrency` | 20 | 每台 agent 的并发上限 |
| `title_timeout_seconds` | 15 | 单域名总抓取预算，最大 15 秒 |
| `rpc_timeout_seconds` | 20 | 给超时结果回传留余量 |
| `failure_percent` | 100 | 标题变化和异常确认共用阈值 |
| `min_healthy_agents` | 1 | 最少有效节点数 |
| `health_interval_seconds` | 30 | 机器健康检查间隔 |
| `state_file` | `data/state.json` | 相对于配置文件的路径 |

每台 agent 检测全部 500 域名，三台并行。每台 20 并发、每域名耗时 15 秒时，抓取预算约 375 秒；这是估算，部署后观察实际轮次日志。超长轮次跳过错过的周期，不叠加任务。

安装到其他目录时使用 `INSTALL_DIR=/usr/local/title_master sh deploy.sh`，之后更新保持相同变量。`state_file` 可使用独立的持久数据目录，脚本会为该目录配置服务用户及写权限，不要填写共享系统目录。目录名支持英文字母、数字、`_`、`-`、`.`、`/`。`TITLE_MASTER_REF=分支名 sh deploy.sh` 可以指定分支。

## 判定与通知

- 默认 100%：有效节点必须返回同一标题才确认变化；两台新标题、一台旧标题时保留上次状态。
- agent 连接/鉴权/API/健康探针失败：整台机器本轮结果剔除，仅通知机器群。中途失败也会剔除该机器此前的观测。
- 网站超时、连接失败、无标题等是有效异常，仍在分母中。异常比例达到阈值才确认；HTTP 500 等错误页面有标题时保留真实标题。
- 所有机器故障或未达到 `min_healthy_agents` 时不改变域名基线。
- 首次正常结果建立基线不通知，首次异常默认通知一次。`123 → 500服务失败 → 500服务失败 → 123` 只产生两次变化通知。
- 多 webhook 分别记录投递结果，只重试失败地址；程序重启可恢复队列。普通 webhook 为至少一次投递，响应丢失等情况可能重试同一事件，事件中包含唯一 ID。

结果保存在 `state_file`，不回写旧 SEO 标题页面。切换完成后如旧 `seo-title-alert.service` 仍在通知，可停用旧通知器以免两套同时发送。

## 手动编译与验证

```sh
go test ./...
go vet ./...
go build -trimpath -o title-master ./cmd/title-master
./title-master -config master.json -check-config
./title-master -config master.json -print-state-path
sh tests/script-smoke.sh master deploy.sh
```

最后一项使用模拟的 Git、Go 和 systemctl 检查部署逻辑，不安装真实服务。`deploy/title-master.service` 是默认路径模板，自动部署会按实际路径生成服务。
