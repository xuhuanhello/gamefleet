#!/bin/sh
# Sets up GameFleet on a fresh Debian or Ubuntu management machine: Docker,
# the compose project in this directory, a console account, HTTPS on the
# domain, and the updater that lets the console install new releases.
# Run it as root, straight from a release:
#
#   curl -fsSL https://github.com/xuhuanhello/gamefleet/releases/latest/download/install.sh |
#     sh -s -- --domain fleet.example.com --email ops@example.com
#
# or from the directory that holds compose.yaml, with --image for an image
# of your own. The domain must point at this machine, and the cloud firewall
# must let in TCP 80 and 443. Running the script again keeps the account and
# the data, and starts whatever is not running.
set -eu

# The release workflow sets VERSION in the copy it publishes.
VERSION=dev
IMAGE_REPO=ghcr.io/xuhuanhello/gamefleet
RELEASES=https://github.com/xuhuanhello/gamefleet/releases

fail() { echo "GameFleet 停下了：$*" >&2; exit 1; }
say() { echo "GameFleet：$*"; }

[ "$(id -u)" = 0 ] || fail "请用 root 执行"
cd "$(dirname "$0")"
if [ ! -f compose.yaml ]; then
	# Run from a release without its files: fetch them into /opt/gamefleet.
	[ "$VERSION" != dev ] || fail "要在放着 compose.yaml 的目录里执行，或者用 Release 里的 install.sh"
	command -v curl >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq curl; } >/dev/null 2>&1
	mkdir -p /opt/gamefleet
	cd /opt/gamefleet
	say "下载 v$VERSION 的部署文件到 /opt/gamefleet"
	curl -fsSL --max-time 60 "$RELEASES/download/v$VERSION/gamefleet-deploy.tar.gz" | tar -xzf - ||
		fail "下载不了部署文件（$RELEASES）"
	exec sh install.sh "$@"
fi

domain='' email='' image='' user=admin
while [ $# -gt 0 ]; do
	case "$1" in
	--domain) domain=$2; shift 2 ;;
	--email) email=$2; shift 2 ;;
	--image) image=$2; shift 2 ;;
	--user) user=$2; shift 2 ;;
	*) echo "不认识的参数 $1" >&2; exit 2 ;;
	esac
done

[ -f compose.yaml ] && [ -f Caddyfile ] && [ -f .env.example ] || fail "要在放着 compose.yaml、Caddyfile 和 .env.example 的目录里执行"
[ -n "$image" ] || [ "$VERSION" = dev ] || image=$IMAGE_REPO:$VERSION
[ -n "$domain" ] && [ -n "$email" ] && [ -n "$image" ] ||
	fail "用法：sh install.sh --domain 域名 --email 证书联系邮箱 [--image 镜像]"

if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
	say "安装 Docker"
	command -v curl >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq curl; } >/dev/null 2>&1
	curl -fsSL --max-time 60 https://get.docker.com -o /tmp/get-docker.sh || fail "下载不了 Docker 的安装脚本（get.docker.com）"
	sh /tmp/get-docker.sh >/tmp/get-docker.log 2>&1 || {
		tail -n 5 /tmp/get-docker.log
		fail "Docker 没装上，原因见上面几行"
	}
fi
if ! docker image inspect "$image" >/dev/null 2>&1; then
	say "下载镜像 $image"
	docker pull -q "$image" >/dev/null 2>&1 || fail "这台机器上没有镜像 $image，也下载不了"
fi

# The updater installs the releases the console asks for (see updater.sh).
# GameFleet and the updater share update/; GameFleet runs as uid 65532.
mkdir -p update
chown root:65532 update
chmod 770 update
update_dir=''
if [ -d /run/systemd/system ] && [ -f gamefleet-updater.service ]; then
	sed "s|@DIR@|$PWD|g" gamefleet-updater.service >/etc/systemd/system/gamefleet-updater.service
	systemctl daemon-reload
	systemctl enable -q gamefleet-updater
	# The updater that runs this script restarts itself once it is done.
	[ "${GAMEFLEET_UPDATER:-}" = 1 ] || systemctl restart gamefleet-updater
	update_dir=/var/lib/gamefleet-update
else
	say "这台机器没有 systemd，不装更新器：新版本要用安装脚本升级"
fi

say "写设置 .env"
cat >.env <<EOF
GAMEFLEET_DOMAIN=$domain
ACME_EMAIL=$email
GAMEFLEET_IMAGE=$image
GAMEFLEET_IMAGE_REPO=$IMAGE_REPO
GAMEFLEET_UPDATE_DIR=$update_dir
$(grep -E '^(CADDY|REGISTRY)_IMAGE=' .env.example)
EOF

# The console password and the authenticator link stay in credentials/ on
# this machine; GameFleet's config holds only the password's hash.
mkdir -p private credentials
chmod 700 credentials
if [ ! -f private/config.json ]; then
	say "建控制台账号 $user"
	umask 077
	od -An -N18 -tx1 /dev/urandom | tr -d ' \n' >credentials/console-password
	account=$(docker run --rm -i --user 0:0 -v "$PWD/credentials:/out" "$image" \
		user -otpauth /out/console-otpauth "$user" <credentials/console-password) || fail "建账号失败"
	cat >private/config.json <<EOF
{
  "listen": "0.0.0.0:8080",
  "database": "/var/lib/gamefleet/gamefleet.db",
  "registry": {"url": "http://registry:5000", "host": "$domain"},
  "drainRetention": "24h",
  "users": [$account]
}
EOF
	umask 022
fi
# GameFleet runs as uid 65532 and reads its config through the group.
chown -R root:65532 private
chmod 750 private
chmod 640 private/config.json

say "启动 GameFleet"
docker compose up -d >/tmp/gamefleet-up.log 2>&1 || {
	tail -n 5 /tmp/gamefleet-up.log
	fail "GameFleet 没启动，原因见上面几行"
}

say "等 HTTPS 证书和 GameFleet 就绪"
# A release must answer with its own version, not the one it replaces.
want=$(echo "${image##*:}" | sed -n 's/^v\{0,1\}\([0-9]*\.[0-9]*\.[0-9]*\)$/\1/p')
ready() {
	# Ask Caddy on this machine: a cloud machine often cannot reach its own public IP.
	health=$(curl -sf --max-time 5 --resolve "$domain:443:127.0.0.1" "https://$domain/healthz") || return 1
	[ -z "$want" ] || echo "$health" | grep -q "\"version\":\"$want\""
}
i=0
until ready; do
	i=$((i + 1))
	if [ $i -ge 36 ]; then
		docker compose logs --tail 8 caddy gamefleet 2>&1 | cut -c 1-300
		fail "3 分钟内 https://$domain 还没就绪。确认域名解析到这台机器，云防火墙放行了 TCP 80 和 443"
	fi
	sleep 5
done

cat <<EOF

GameFleet：装好了，控制台在 https://$domain
控制台账号是 $user。密码和验证器链接只在这台机器上：
  $PWD/credentials/console-password
  $PWD/credentials/console-otpauth
在你自己的终端里取出来，把链接加进验证器 App；能登录以后就可以删掉这两个文件。
EOF
