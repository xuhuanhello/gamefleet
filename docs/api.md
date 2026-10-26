# 接口、命令行和配置

GameFleet 对外只有一个地址，比如 `https://fleet.example.com`：控制台、给机器调用的 API、镜像仓库都在这个地址下。

## API key

机器调用 GameFleet 都用 API key，放在 `Authorization: Bearer <key>` 里；镜像仓库按 Docker 的习惯用 Basic 认证，用户名随便填，密码就是 key。每把 key 有一个或几个权限：

| 权限 | 能做什么 | 通常谁用 |
|---|---|---|
| `push` | 推镜像、登记构建，也能拉镜像 | CI |
| `pull` | 拉镜像 | 每个集群（key 名 `pull-集群名`，接入时自动生成，不要吊销） |
| `allocate` | 读路由、分配战斗服、查对局 | 你的匹配服务 |
| `gm` | 查对局、结束一局、删除战斗服 | 你的 GM 工具 |

一个调用方需要几种权限时，给它一把带几种权限的 key，比如 `allocate,gm`。在控制台"设置 → API key"里新建和吊销；新建的 key 只显示这一次。GameFleet 只存 key 的 SHA-256。换 key 时先发新的，再吊销旧的，中间不会断。

## 接口

除了 `/healthz`，都要带 key。出错时返回 `{"error": "代码"}`，有的还带给人看的 `message`。

| 接口 | 权限 | 返回 |
|---|---|---|
| `GET /healthz` | – | `{"status":"ok","version":"2.1.0"}`；从源码编译的版本号是 `dev` |
| `POST /api/v1/builds` | `push` | `201` 和新构建；镜像已经登记过时返回 `200` 和那个构建 |
| `GET /api/v1/routes?game=` | `allocate` | 这个游戏的路由：通道、联机版本、构建、部署了的地区 |
| `POST /api/v1/allocations` | `allocate` | `201` 和分到的战斗服；同一个 matchId 再要一次，返回 `200` 和同一个战斗服 |
| `GET /api/v1/rooms/{名字}` | `allocate`、`gm` | 这个战斗服，或者 `404 room_not_found` |
| `GET /api/v1/rooms?matchId=` | `allocate`、`gm` | `{"rooms": [...]}`，零个或一个：跑这一局的战斗服 |
| `POST /api/v1/matches/{matchId}/end` | `gm` | 让跑这一局的战斗服结束它，返回 `202` 和 `{"roomId", "matchId"}`；没有战斗服在跑时返回 `404 room_not_found`。见[战斗服约定](game-server.md#被要求结束一局) |
| `DELETE /api/v1/rooms/{名字}` | `gm` | 删掉这个战斗服，返回 `202`。上面所有的对局都没了 |
| `/v2/…` | `push`、`pull` | 镜像仓库协议。GameFleet 检查 key 后转给后面的 registry |

### 登记构建

```json
POST /api/v1/builds
{"repository": "billiards", "tag": "142", "digest": "sha256:…", "note": "可选，最多 500 字"}
```

`repository` 是游戏名，也是镜像仓库名。`tag` 和 `digest` 给一个就行。返回构建号、版本、联机版本、digest 和全部镜像标签。出错：`404 image_not_found`（仓库里没有这个镜像）、`422 image_unusable`（缺必填标签或端口不对）、`400 invalid_request`。

### 读路由

你的匹配服务可以先读路由，判断一个客户端能不能排队：它的联机版本有没有正式路由（没有就提示更新），它选的地区有没有部署（没有就提示换地区），测试玩家有没有灰度可进。

```json
GET /api/v1/routes?game=billiards
{"routes": [
  {"game": "billiards", "channel": "stable", "compat": "dm-v7", "build": 5, "regions": ["sg", "us"]},
  {"game": "billiards", "channel": "canary", "compat": "dm-v7", "build": 6, "regions": ["us"]}
]}
```

### 分配战斗服

匹配服务配好一局后调用：

```json
POST /api/v1/allocations
{
  "matchId": "8c1f0c7e-5d1e-4f4c-9a59-3c3b8e0f2a11",
  "game": "billiards",
  "region": "us",
  "channel": "stable",
  "compat": "dm-v7",
  "players": [{"userId": "u-1", "seat": 0}, {"userId": "u-2", "seat": 1}]
}
```

`matchId` 由你生成，每局一个：字母、数字和 `-_.`，开头和结尾是字母或数字，最长 57 个字符，UUID 正好合适。`channel` 由你决定：测试玩家传 `canary`，其他人传 `stable`。返回分到的战斗服，查对局的接口返回的也是这个样子：

```json
{
  "roomId": "billiards-1-9q4jb-qwdr8",
  "cluster": "us-1",
  "state": "Allocated",
  "address": "203.0.113.20",
  "port": 20370,
  "fleet": "billiards-1",
  "build": "1",
  "matchId": "8c1f0c7e-5d1e-4f4c-9a59-3c3b8e0f2a11",
  "players": [{"userId": "u-1", "seat": 0}, {"userId": "u-2", "seat": 1}]
}
```

`roomId` 是战斗服（GameServer）的名字，玩家连 `address:port`。一个战斗服同时跑多局，`matchId` 和 `players` 说的是请求里的这一局。

GameFleet 在这个地区、路由指向的部署里找一个还有空位的战斗服：先找已经在跑对局的，空位最少的优先，都满了才用空闲的；所在机器正在下线的不用。同一个集群里一次只发一个分配，一个集群每秒大约能分 20 局。

| 状态 | 代码 | 意思 |
|---|---|---|
| 400 | `invalid_request` | 请求格式不对 |
| 404 | `route_not_found` | 这个通道和联机版本没有路由，或者路由指向的构建没有部署在这个地区 |
| 409 | `no_capacity` | 这个部署的战斗服都满了，也到了上限；或者新开的还没启动好 |
| 503 | `cluster_unavailable` | 连不上集群，或者集群拒绝了 GameFleet |

同一个 matchId 重试是安全的：已经分配过，就返回原来的战斗服。

### GameFleet 重启的时候

GameFleet 更新或重启时，有几秒到十几秒不应答：连不上，或者 Caddy 返回 502。正在打的对局不受影响，玩家只连战斗服。匹配服务这样做，重启对玩家就只是多等一会儿：

- **分配失败就用同一个 matchId 重试**：连不上、502、503 都重试，间隔逐步拉长（比如 0.5、1、2、2……秒），直到这一局愿意等的上限，比如 30 秒。`409`、`404` 不用重试。
- **缓存路由**：GameFleet 不应答时沿用上次读到的路由，过几秒再问。路由只在有人改的时候才变。

### 判断一局还在不在

断线重连时，你的游戏后端可以用 `GET /api/v1/rooms?matchId=` 查这一局还在不在：查不到，就是战斗服没了（比如机器坏了），这一局可以按作废处理。战斗服在结果交出去以后才把一局删掉，所以结果还没到时，一局总能查到。

## 命令行

`gamefleet` 程序除了跑服务，还有几个管理命令。在管理机上这样执行：

```sh
cd /opt/gamefleet && docker compose exec -T gamefleet /gamefleet 命令 参数 < 输入
```

| 命令 | 作用 |
|---|---|
| `key 名字 权限[,权限]` | 新建一把 key，值从标准输入读。用于值已经在别处生成好的情况，比如 CI 的 key |
| `key -sha256 名字 权限` | 同上，但标准输入是 key 的 SHA-256（十六进制），key 本身留在调用方那里 |
| `var 游戏 变量名` | 设一项运行时配置，值从标准输入读，末尾的换行会去掉。最多 15 秒后写进所有集群 |
| `user 名字` | 生成一个控制台账号的配置条目，密码从标准输入读。加 `-otpauth 文件` 把验证器链接写进文件，不打印出来 |
| `cluster add` | 用标准输入里的连接信息 JSON 接入一个不是用 GameFleet 的脚本装的集群。字段：`name`、`region`、`server`、`caBase64`、`token`、`namespace`、`joinUrl`、`joinToken` |

命令行做的改动和控制台一样记进审计日志，操作者显示为 `gamefleet key`、`gamefleet var` 这样的名字。

## 配置文件

`/opt/gamefleet/private/config.json`，由安装脚本生成，平时不用改。不认识的字段会被拒绝。改完要 `docker compose restart gamefleet`。

| 字段 | 意思 |
|---|---|
| `listen` | GameFleet 监听的地址，`0.0.0.0:8080`，只对 Caddy 开放 |
| `database` | SQLite 数据库文件，`/var/lib/gamefleet/gamefleet.db`，在 Docker 卷 `gamefleet_gamefleet_data` 里 |
| `registry.url` | GameFleet 访问镜像仓库的地址，`http://registry:5000` |
| `registry.host` | GameFleet 的公网域名。集群从这里拉镜像，新机器从这里下载安装脚本 |
| `drainRetention` | 排空完的部署保留多久才删，默认 `24h`。保留期内可以一步回滚 |
| `users` | 控制台账号：用户名、密码的哈希、验证器密钥 |

环境变量（在 `compose.yaml` 里设好）：

| 变量 | 意思 |
|---|---|
| `GAMEFLEET_UPDATE_DIR` | 和更新器共享的目录；空着就不能在控制台里更新 |
| `GAMEFLEET_RELEASES` | 从哪个 GitHub 仓库检查新版本，默认 `xuhuanhello/gamefleet`；设成空字符串就不检查 |

数据库里只存 K8s 里没有的东西：集群的连接信息、构建、部署、路由、API key（只有哈希）、运行时配置、审计日志。节点、Pod、战斗服、日志和事件都是打开页面时实时从 K8s 读的。
