#!/bin/sh
# Joins this machine to a GameFleet cluster as a worker for game servers.
# Get the command from the cluster's page in the GameFleet console:
#
#   curl -sfL {{.Base}}/install/node.sh | sh -s -- --server URL --token TOKEN
#
# Run it as root on a fresh Debian or Ubuntu VPS that can reach the server
# address. The machine shows up in the console seconds later.
set -eu

K3S_VERSION='{{.K3s}}'
GAMEFLEET='{{.Base}}'
MIN_PORT={{.MinPort}}
MAX_PORT={{.MaxPort}}

server='' token='' public_ip=''
while [ $# -gt 0 ]; do
	case "$1" in
	--server) server=$2; shift 2 ;;
	--token) token=$2; shift 2 ;;
	--public-ip) public_ip=$2; shift 2 ;;
	*) echo "不认识的参数 $1" >&2; exit 2 ;;
	esac
done

fail() { echo "GameFleet 停下了：$*" >&2; exit 1; }
say() { echo "GameFleet：$*"; }

case "$server" in https://*) ;; *) fail "--server 要是集群的 https 地址；请从控制台复制完整的命令" ;; esac
[ -n "$token" ] || fail "缺少 --token；请从控制台复制完整的命令"
[ "$(id -u)" = 0 ] || fail "请用 root 执行"
[ "$(uname -m)" = x86_64 ] || fail "只支持 x86_64 的机器"
command -v systemctl >/dev/null || fail "需要 systemd"
command -v curl >/dev/null || fail "需要 curl"
# A run that stopped after K3s was installed, waiting for the machine to get
# ready say, leaves it installed: it is removed by hand before trying again.
[ ! -e /usr/local/bin/k3s ] || fail "这台机器已经装了 K3s，可能是上次加入到一半停下了。先在这台机器上执行 /usr/local/bin/k3s-agent-uninstall.sh，再重试"

if [ -z "$public_ip" ]; then
	public_ip=$(curl -sf --max-time 10 "$GAMEFLEET/install/ip") || fail "查不到这台机器的公网 IP，请加上 --public-ip"
fi
echo "$public_ip" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' || fail "公网 IP '$public_ip' 不像 IPv4 地址"

# K3s names the node after the host name, in lower case.
node=$(hostname | tr 'A-Z' 'a-z')
say "以 $node（$public_ip）的身份加入 $server"
say "下载并安装 K3s $K3S_VERSION"
curl -sfL --max-time 60 https://get.k3s.io -o /tmp/k3s-install.sh || fail "下载不了 K3s 的安装脚本（get.k3s.io）"
# The installer would start the agent and wait for it, forever when the
# server turns the agent away: start it here without waiting instead.
INSTALL_K3S_SKIP_START=true INSTALL_K3S_VERSION="$K3S_VERSION" K3S_URL="$server" K3S_TOKEN="$token" \
	sh /tmp/k3s-install.sh agent --node-external-ip "$public_ip" --node-label gamefleet.io/pool=game >/tmp/k3s-install.log 2>&1 || {
	tail -n 5 /tmp/k3s-install.log
	fail "K3s 没装上，原因见上面几行"
}
systemctl start --no-block k3s-agent

# Wait for this machine's node to be ready, asking with the kubelet's own
# credentials.
say "等这台机器在集群里就绪"
ready() {
	[ "$(/usr/local/bin/k3s kubectl --kubeconfig /var/lib/rancher/k3s/agent/kubelet.kubeconfig get node "$node" \
		-o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = True ]
}
i=0
until ready; do
	i=$((i + 1))
	if [ $i -ge 36 ]; then
		journalctl -u k3s-agent -n 12 --no-pager -o cat 2>/dev/null | cut -c 1-300
		fail "3 分钟内这台机器没有在集群里就绪，K3s 最近的日志见上面"
	fi
	sleep 5
done

cat <<EOF

GameFleet：这台机器已经加入，Agones 随即在上面开战斗服。
玩家通过 UDP $MIN_PORT-$MAX_PORT 连接战斗服：云防火墙要对所有地址放行这段端口。
EOF
