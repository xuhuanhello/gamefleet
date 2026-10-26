# 安装、升级和备份

## 准备

- **管理机：** 一台 VPS，2 核 4 GB 够用，Debian 或 Ubuntu，x86_64，有 root。GameFleet、镜像仓库和 Caddy 都跑在这台上。
- **域名：** 一个解析到管理机的域名，比如 `fleet.example.com`。控制台、API、镜像仓库都在这个域名下，Caddy 自动申请 HTTPS 证书。
- **防火墙：** 管理机对所有地址放行 TCP 80（申请证书）和 443。

集群的机器另外准备，见[集群和机器](clusters.md)。

## 安装

在管理机上用 root 执行：

```sh
curl -fsSL https://github.com/xuhuanhello/gamefleet/releases/latest/download/install.sh |
  sh -s -- --domain fleet.example.com --email 你的邮箱
```

`--email` 是申请证书时留给证书机构的联系邮箱。脚本依次：

1. 把这个版本的部署文件下载到 `/opt/gamefleet`；
2. 没有 Docker 就装上；拉 GameFleet 的镜像；
3. 装更新器（systemd 服务 `gamefleet-updater`），控制台里的在线更新靠它；
4. 第一次运行时建控制台账号 `admin`：随机密码写进 `credentials/console-password`，验证器链接写进 `credentials/console-otpauth`；
5. 启动 Caddy、GameFleet 和镜像仓库，等 `https://域名/healthz` 报出这个版本号，最多 3 分钟。

重复运行是安全的：账号、数据和密钥都保留，没运行的重新运行起来。想用自己编译的镜像，加 `--image 镜像`。

## 登录

在你自己的终端里取出密码，存进密码管理器；把验证器链接加进验证器 App：

```sh
ssh root@管理机 cat /opt/gamefleet/credentials/console-password
ssh root@管理机 cat /opt/gamefleet/credentials/console-otpauth | qrencode -t ansiutf8
```

打开 `https://fleet.example.com`，填用户名 `admin`、密码和 App 上的验证码。验证码每 30 秒换一次，每个只能用一次。登录一次管 12 小时；登录状态在内存里，GameFleet 重启后要重新登录。

能登录以后，就可以删掉管理机上的这两个文件：`rm /opt/gamefleet/credentials/console-password /opt/gamefleet/credentials/console-otpauth`。

**加账号、重设密码：** 账号写在 `/opt/gamefleet/private/config.json` 的 `users` 里。生成一条新账号（密码从标准输入读，验证器链接写进文件）：

```sh
cd /opt/gamefleet
docker run --rm -i --user 0:0 -v "$PWD/credentials:/out" "$(sed -n 's/^GAMEFLEET_IMAGE=//p' .env)" \
  user -otpauth /out/新名字-otpauth 新名字 < 密码文件
```

把输出的那条账号加进 `users`（同名的就替换掉），然后 `docker compose restart gamefleet`。验证器链接在 `credentials/新名字-otpauth`。

## 在线更新

有新版本时，控制台左下角显示"新版本"。点开是"设置 → 更新"：

![更新页](images/update.png)

1. 看更新说明，或者点链接去 GitHub 的 Release 页面看完整的改动；
2. 点"下载"：更新器把新版本的镜像拉到管理机上，页面显示进度，这一步不影响正在运行的 GameFleet；
3. 点"重启并更新"并确认：GameFleet 先把数据库备份到 `backups/`，更新器换上新版本的部署文件，用新镜像重跑安装脚本。GameFleet 停十几秒；页面一直等着，新版本起来后写出结果，10 秒后自动刷新。重启会让所有人退出登录，刷新后重新登录即可。

这期间分配接口不应答：匹配服务按 matchId 重试的话，新的对局只是多等一会儿，见[接口](api.md#gamefleet-重启的时候)；正在打的对局不受影响。新版本 3 分钟内没有启动起来，更新器会自动换回原来的版本，并在更新页上写明原因。换回原来的版本也可能失败，比如新版本已经改了数据库结构、旧版本不认：这时更新页会直说，并写出更新前那份数据库备份的文件名，照下面"备份和恢复"把它换回去，再用原来的版本运行安装脚本。

管理机在下载或更新到一半时重启，更新器起来后会把这次标成失败，页面上可以再点一次。

GameFleet 只在有人用控制台时检查新版本：打开或刷新更新页、点"检查更新"时马上检查；打开别的页面时，距上次检查超过一小时才在后台查一次，用来在左下角提示。没人打开控制台时，它不联网检查。检查只发给 `api.github.com`，不带任何你的数据。

**更新器做了什么：** GameFleet 在对公网开放的容器里，拿不到 Docker 的控制权。它只在 `/opt/gamefleet/update/` 里写一个请求（下载或更新，加一个版本号）；更新器读到以后，自己拼出镜像地址 `ghcr.io/xuhuanhello/gamefleet:版本` 去拉，所以从控制台只能装上官方发布的版本。

这不是挡住入侵者的一道墙。GameFleet 握着各个集群的凭据，能在集群里创建任意容器；集群的控制面和管理机是同一台机器时，攻破 GameFleet 的人能借集群拿到这台机器。所以 GameFleet 被攻破，要当作管理机和各个集群都失守了来处理。控制台的密码、验证器和 API key 要保管好。

**更新说明里提到要更新集群上的设置时：** 有的版本会改 GameFleet 在集群里的权限或 Agones 的参数，这些 GameFleet 自己改不了，要在每个集群的控制面上执行，做法见[集群和机器](clusters.md#更新集群上的权限和-agones-设置)。

## 手动升级

没有 systemd、没装更新器，或者想指定版本时，把新版本的部署文件解到原来的目录，再运行一次安装脚本：

```sh
cd /opt/gamefleet
curl -fsSL https://github.com/xuhuanhello/gamefleet/releases/download/v2.1.0/gamefleet-deploy.tar.gz | tar -xzf -
sh install.sh --domain fleet.example.com --email 你的邮箱
```

不支持降级：新版本可能改了数据库结构。要回到旧版本，先停 GameFleet，用升级前的备份换回数据库，再用旧版本的部署文件和镜像运行安装脚本。

## 备份和恢复

| 东西 | 在哪 | 丢了会怎样 |
|---|---|---|
| 数据库 | Docker 卷 `gamefleet_gamefleet_data` 里的 `gamefleet.db` | 构建、部署、路由、集群连接、API key、运行时配置和审计日志都没了。集群本身和正在打的对局不受影响，但要重新接入集群、重新登记 key |
| 配置 | `/opt/gamefleet/private/config.json` | 控制台账号没了，重新运行安装脚本会新建一个 |
| 镜像仓库 | Docker 卷 `gamefleet_registry_data` | 已登记的构建拉不到镜像，要让 CI 重新推 |

在线更新前，GameFleet 自动把数据库备份到同一个卷里的 `backups/gamefleet-旧版本-时间.db`，保留最近 5 份。

**手动备份：** GameFleet 要停几秒，拷出来的文件才完整。这几秒里分配接口不应答，正在打的对局不受影响。

```sh
cd /opt/gamefleet && umask 077 && d=backup/$(date +%Y%m%d-%H%M%S) && mkdir -p $d &&
  docker compose stop gamefleet && docker compose cp gamefleet:/var/lib/gamefleet/. $d/; docker compose start gamefleet
```

**恢复：** 停掉 GameFleet，删掉数据库旁边的两个临时文件（`-wal`、`-shm`，不删的话 SQLite 会把它们套到恢复的数据库上），再把备份放回卷里，属主改成 GameFleet 的用户（65532）：

```sh
cd /opt/gamefleet && v=$(docker volume inspect -f "{{.Mountpoint}}" gamefleet_gamefleet_data) &&
  docker compose stop gamefleet && rm -f $v/gamefleet.db-wal $v/gamefleet.db-shm &&
  install -o 65532 -g 65532 -m 644 备份的文件 $v/gamefleet.db && docker compose start gamefleet
```

备份之后才做的改动（新构建、新部署、新的 key）都没有了。集群里多出来的部署，GameFleet 当作不认识的部署处理：等上面的对局打完就删掉。

## 管理机上的文件

`/opt/gamefleet` 下：

| 路径 | 内容 |
|---|---|
| `compose.yaml`、`Caddyfile`、`install.sh`、`updater.sh`、`gamefleet-updater.service`、`.env.example` | 部署文件，随版本更新 |
| `.env` | 域名、证书邮箱、正在用的镜像 |
| `private/config.json` | GameFleet 的配置和控制台账号，见[配置](api.md#配置文件) |
| `credentials/` | 第一次安装时的控制台密码和验证器链接，登录以后可以删 |
| `update/` | GameFleet 和更新器之间的请求、进度和日志 |

日志：`docker compose logs gamefleet`；更新器的日志：`journalctl -u gamefleet-updater`。
