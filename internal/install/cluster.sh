#!/bin/sh
# Turns this machine into a new GameFleet cluster: K3s server, Agones, the
# namespace for game servers and a restricted account for GameFleet. The
# machine is the control plane and runs game servers too; add more machines
# later from the console.
#
# Get the command with its join code from the GameFleet console (集群 → 接入集群):
#
#   curl -sfL {{.Base}}/install/cluster.sh | sh -s -- --code CODE
#
# It reports its progress to the console, and when K3s and Agones are up
# GameFleet connects to the cluster by itself. Running it again after a
# failure picks up where it stopped. Run it as root on a fresh Debian or
# Ubuntu VPS.
set -eu

K3S_VERSION='{{.K3s}}'
AGONES_VERSION='{{.Agones}}'
GAMEFLEET='{{.Base}}'
NAMESPACE='{{.Namespace}}'
MIN_PORT={{.MinPort}}
MAX_PORT={{.MaxPort}}
CONFIG=/etc/rancher/k3s/config.yaml

code='' public_ip='' control_plane_only=''
while [ $# -gt 0 ]; do
	case "$1" in
	--code) code=$2; shift 2 ;;
	--public-ip) public_ip=$2; shift 2 ;;
	--control-plane-only) control_plane_only=1; shift ;;
	*) echo "不认识的参数 $1" >&2; exit 2 ;;
	esac
done

fail() { echo "GameFleet 停下了：$*" >&2; exit 1; }
say() { echo "GameFleet：$*"; }

echo "$code" | grep -Eq '^[a-z2-7]{20,64}$' || fail "缺少 --code 或者不对；请从控制台复制完整的命令"
[ "$(id -u)" = 0 ] || fail "请用 root 执行"
[ "$(uname -m)" = x86_64 ] || fail "只支持 x86_64 的机器"
command -v systemctl >/dev/null || fail "需要 systemd"
command -v curl >/dev/null || fail "需要 curl"
memory_kb=$(awk '/MemTotal/ { print $2 }' /proc/meminfo)
[ "$memory_kb" -ge 1800000 ] || fail "至少要 2 GB 内存"

# The join code names the cluster and its region.
join=$(curl -sf --max-time 20 "$GAMEFLEET/install/clusters/$code") || fail "加入码不对或已过期，请在控制台重新生成"
name=$(echo "$join" | sed -n 's/.*"name":"\([a-z0-9-]*\)".*/\1/p')
region=$(echo "$join" | sed -n 's/.*"region":"\([a-z0-9-]*\)".*/\1/p')
[ -n "$name" ] && [ -n "$region" ] || fail "GameFleet 的回答看不懂：$join"

report() {
	curl -sS --max-time 90 -o /tmp/gamefleet-report -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
		--data "$1" "$GAMEFLEET/install/clusters/$code" || echo 000
}
# json makes text fit inside a JSON string.
json() { printf '%s' "$1" | tr '\n\r\t' '   ' | sed 's/\\/\\\\/g; s/"/\\"/g'; }
shown='' shown_step='' shown_problem=''
# status STAGE PART PARTS STEP [PROBLEM] tells the console which step of a
# stage the install is on, with PART of PARTS done, and what holds it up. It
# prints the step and the problem when they change.
status() {
	[ "$*" != "$shown" ] || return 0
	[ "$4" = "$shown_step" ] || say "$4"
	[ -z "${5:-}" ] || [ "$5" = "$shown_problem" ] || say "$5"
	shown="$*" shown_step=$4 shown_problem=${5:-}
	report "$(printf '{"stage":"%s","part":%s,"parts":%s,"step":"%s","problem":"%s"}' \
		"$1" "$2" "$3" "$(json "$4")" "$(json "${5:-}")")" >/dev/null
}

if [ -z "$public_ip" ]; then
	public_ip=$(curl -sf --max-time 10 "$GAMEFLEET/install/ip") || fail "查不到这台机器的公网 IP，请加上 --public-ip"
fi
echo "$public_ip" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' || fail "公网 IP '$public_ip' 不像 IPv4 地址"
say "在 $public_ip 上安装集群 $name（地区 $region）"

MANIFESTS=/var/lib/rancher/k3s/server/manifests
# An earlier run wrote the config before it installed K3s, and may have
# stopped in between, a download failing say. It counts as installed only
# when the binary and the service are there too; otherwise the installer
# runs again, with the config, and the join token, of the earlier run.
configured='' installed=''
if grep -q '^# gamefleet cluster' "$CONFIG" 2>/dev/null; then
	configured=1
	if [ -x /usr/local/bin/k3s ] && [ -f /etc/systemd/system/k3s.service ]; then
		installed=1
		say "上次已经装过 K3s，接着往下装"
	else
		say "上次写好了配置，K3s 没装完，这次接着装"
	fi
else
	[ ! -e /usr/local/bin/k3s ] || fail "这台机器已经装了别的 K3s，请先执行 k3s-uninstall.sh"
fi

# K3s applies every file in this directory when it starts and again when one
# changes, so running this script again brings them up to date too.
mkdir -p "$MANIFESTS"
cat >"$MANIFESTS/gamefleet.yaml" <<'EOF'
{{.Manifests}}
EOF
cat >/tmp/gamefleet-agones.yaml <<EOF
apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: agones
  namespace: kube-system
spec:
  repo: https://agones.dev/chart/stable
  chart: agones
  version: $AGONES_VERSION
  targetNamespace: agones-system
  createNamespace: true
  # Agones' CRDs carry fields that server-side apply rejects.
  serverSide: "false"
  valuesContent: |-
    agones:
      allocator: {install: false}
      ping: {install: false}
      crds: {cleanupOnDelete: false}
      controller:
        replicas: 1
        persistentLogs: false
        resources: {requests: {cpu: 100m, memory: 256Mi}, limits: {memory: 512Mi}}
      extensions:
        replicas: 1
        persistentLogs: false
        resources: {requests: {cpu: 100m, memory: 128Mi}, limits: {memory: 512Mi}}
        # Extensions serves allocations. Between batches it waits this long
        # (500ms by default), and GameFleet sends one allocation at a time.
        allocationBatchWaitTime: 50ms
      image:
        sdk: {cpuRequest: 30m, cpuLimit: 100m, memoryRequest: 32Mi, memoryLimit: 64Mi}
    gameservers:
      namespaces: [$NAMESPACE]
      minPort: $MIN_PORT
      maxPort: $MAX_PORT
EOF
# When the chart changes, K3s replaces its install job: attempts from before
# then say nothing about this run.
agones_since=0
if ! cmp -s /tmp/gamefleet-agones.yaml "$MANIFESTS/agones.yaml"; then
	agones_since=$(date +%s)
	mv /tmp/gamefleet-agones.yaml "$MANIFESTS/agones.yaml"
fi
rm -f /tmp/gamefleet-agones.yaml

kubectl() { /usr/local/bin/k3s kubectl "$@"; }
wait_for() {
	what=$1; shift
	i=0
	until "$@" >/dev/null 2>&1; do
		i=$((i + 1))
		[ $i -lt 120 ] || fail "等$what超时了"
		sleep 5
	done
}

# network_check prints what this machine's network looks like from inside
# the cluster, to tell why something there cannot reach the internet. A pod
# made from the Helm job's image, which is already here, asks each DNS server
# in turn for agones.dev.
network_check() {
	api=$(kubectl get endpoints kubernetes -o jsonpath='{.subsets[0].addresses[0].ip}:{.subsets[0].ports[0].port}' 2>/dev/null)
	if [ -z "$api" ] || [ "$api" = : ]; then
		say "网络自检：查不到集群 API 登记的地址"
	elif curl -sk -o /dev/null --max-time 5 "https://$api/"; then
		say "网络自检：集群 API 登记在 $api，本机连得上"
	else
		say "网络自检：集群 API 登记在 $api，本机连不上它，集群里的程序也就连不上 API"
	fi
	say "网络自检：本机的 DNS 服务器 $(awk '/^nameserver/ { printf "%s ", $2 }' /etc/resolv.conf)"
	[ ! -f /var/lib/rancher/k3s/agent/etc/resolv.conf ] ||
		say "网络自检：K3s 给容器用的 DNS 服务器 $(awk '/^nameserver/ { printf "%s ", $2 }' /var/lib/rancher/k3s/agent/etc/resolv.conf)"
	if getent hosts agones.dev >/dev/null; then say "网络自检：本机能解析 agones.dev"; else say "网络自检：本机也解析不了 agones.dev"; fi
	coredns=$(kubectl -n kube-system get pods -l k8s-app=kube-dns -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)
	say "网络自检：CoreDNS $(kubectl -n kube-system get pods -l k8s-app=kube-dns -o jsonpath='{.items[0].status.phase}，就绪 {.items[0].status.containerStatuses[0].ready}，重启 {.items[0].status.containerStatuses[0].restartCount} 次' 2>/dev/null)"
	kubectl -n kube-system logs -l k8s-app=kube-dns --tail=4 2>/dev/null | cut -c 1-300 | while IFS= read -r line; do say "网络自检：CoreDNS 日志 $line"; done
	say "网络自检：ip_forward=$(sysctl -n net.ipv4.ip_forward 2>/dev/null) bridge-nf-call-iptables=$(sysctl -n net.bridge.bridge-nf-call-iptables 2>/dev/null)$(command -v docker >/dev/null && echo '，装了 Docker')"
	say "网络自检：网卡 $(ip -4 -o addr show scope global 2>/dev/null | awk '{ printf "%s %s  ", $2, $4 }')"
	image=$(kubectl -n kube-system get job helm-install-agones -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)
	if [ -z "$image" ]; then
		say "网络自检：机器上还没有能用来测试的镜像，跳过容器里的检查"
		return 0
	fi
	kubectl -n kube-system delete pod gamefleet-netcheck --ignore-not-found >/dev/null 2>&1
	timeout 150 /usr/local/bin/k3s kubectl -n kube-system run gamefleet-netcheck --image="$image" --restart=Never --rm -i --quiet --command -- \
		sh -c 'for s in "$@"; do timeout 6 nslookup agones.dev "$s" >/dev/null 2>&1 && echo "$s 能用" || echo "$s 没回答"; done' \
		check 10.43.0.10 $coredns "$(awk '/^nameserver/ { print $2; exit }' /etc/resolv.conf)" 1.1.1.1 2>&1 |
		while IFS= read -r line; do say "网络自检：容器里问 DNS 服务器 $line"; done
	kubectl -n kube-system delete pod gamefleet-netcheck --ignore-not-found >/dev/null 2>&1
}

# The K3s config is written on every run too, keeping the token machines
# join with, and K3s restarts when it changed.
private_ip=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
echo "$private_ip" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' || fail "查不到这台机器的内网 IP（ip route get 1.1.1.1）"
if [ -n "$configured" ]; then
	agent_token=$(sed -n 's/^agent-token: //p' "$CONFIG")
else
	# A separate token for machines that join later, so it cannot make servers.
	agent_token=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
fi
# Game servers go only to machines labelled for them.
labels="\"gamefleet.io/pool=game\", \"gamefleet.io/cluster=$name\""
[ -z "$control_plane_only" ] || labels="\"gamefleet.io/cluster=$name\""
mkdir -p /etc/rancher/k3s
umask 077
cat >/tmp/gamefleet-k3s.yaml <<EOF
# gamefleet cluster $name
node-external-ip: $public_ip
# Pods reach the API server at the address it advertises, which defaults to
# the external IP. A cloud machine often cannot reach its own public IP (it
# is NAT in front of it), so advertise the private one.
advertise-address: $private_ip
tls-san: ["$public_ip"]
disable: [traefik, servicelb]
agent-token: $agent_token
secrets-encryption: true
write-kubeconfig-mode: "0600"
node-label: [$labels]
# This machine runs the control plane and game servers: keep some CPU and
# memory for K3s itself.
kubelet-arg: ["system-reserved=cpu=300m,memory=512Mi"]
EOF
umask 022
config_changed=''
if ! cmp -s /tmp/gamefleet-k3s.yaml "$CONFIG"; then
	mv /tmp/gamefleet-k3s.yaml "$CONFIG"
	config_changed=1
fi
rm -f /tmp/gamefleet-k3s.yaml

if [ -z "$installed" ]; then
	status k3s 0 3 "下载并安装 K3s $K3S_VERSION"
	curl -sfL --max-time 60 https://get.k3s.io -o /tmp/k3s-install.sh || fail "下载不了 K3s 的安装脚本（get.k3s.io）"
	# The installer would start K3s and wait for it, forever when it cannot
	# start: start it here without waiting, and watch it below.
	INSTALL_K3S_SKIP_START=true INSTALL_K3S_VERSION="$K3S_VERSION" sh /tmp/k3s-install.sh server >/tmp/k3s-install.log 2>&1 || {
		tail -n 5 /tmp/k3s-install.log
		fail "K3s 没装上，原因见上面几行"
	}
	systemctl start --no-block k3s
elif [ -n "$config_changed" ]; then
	# Stop the containers too: a plain restart can leave a container that was
	# starting at that moment running unseen, holding its ports. No games run
	# before the cluster has joined; on a cluster joining again, games stop.
	status k3s 0 3 "按新的配置重启 K3s"
	/usr/local/bin/k3s-killall.sh >/dev/null 2>&1 || true
	systemctl start --no-block k3s
else
	# An earlier run may have stopped after installing K3s, before starting
	# it. Starting a running K3s does nothing.
	systemctl start --no-block k3s
fi

status k3s 1 3 "等 K3s 启动"
i=0
until kubectl wait --for=condition=Ready node --all --timeout=5s >/dev/null 2>&1; do
	i=$((i + 1))
	if [ $i -ge 30 ]; then
		journalctl -u k3s -n 12 --no-pager -o cat 2>/dev/null | cut -c 1-300
		fail "K3s 5 分钟还没启动好，最近的日志见上面"
	fi
	sleep 5
done
# K3s sets node-label only when a node first registers. Set the labels here
# too, so a cluster installed before them, or rerun with other options, gets
# them as well.
node=$(hostname | tr '[:upper:]' '[:lower:]')
kubectl label node "$node" --overwrite "gamefleet.io/cluster=$name" >/dev/null || fail "给本机的节点 $node 打不上标签"
if [ -z "$control_plane_only" ]; then
	kubectl label node "$node" --overwrite gamefleet.io/pool=game >/dev/null
else
	kubectl label node "$node" gamefleet.io/pool- >/dev/null 2>&1
fi
# CoreDNS gets ready once it reaches the API server from inside the cluster,
# which Agones and the game servers need too.
status k3s 2 3 "等集群里的网络和 DNS 就绪"
i=0
until [ "$(kubectl -n kube-system get deployment coredns -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = 1 ]; do
	i=$((i + 1))
	if [ $i -ge 36 ]; then
		network_check || true
		fail "集群里的 DNS（CoreDNS）3 分钟还没就绪，原因见上面的网络自检"
	fi
	sleep 5
done

# pull_problem NAMESPACE POD prints why the pod's image does not download.
pull_problem() {
	kubectl -n "$1" get "$2" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}|{.status.containerStatuses[0].state.waiting.message}' 2>/dev/null |
		awk -F'|' '$1 ~ /ErrImagePull|ImagePullBackOff/ { print substr($2, 1, 240) }'
}

# pulled prints, once each, the Agones images that finished downloading.
: >/tmp/gamefleet-pulled
pulled() {
	kubectl -n agones-system get events --field-selector reason=Pulled -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null |
		sed -n 's|^Successfully pulled image "[^"]*/\([^"/]*\)" in \([^ ]*\) .*Image size: \([0-9]*\) bytes.*|\1 \2 \3|p' |
		awk '{ printf "下载好了镜像 %s：%.0f MB，用了 %s\n", $1, $3 / 1048576, $2 }' >/tmp/gamefleet-pulled-now
	grep -vxF -f /tmp/gamefleet-pulled /tmp/gamefleet-pulled-now | while IFS= read -r line; do say "$line"; done
	sort -u -o /tmp/gamefleet-pulled /tmp/gamefleet-pulled /tmp/gamefleet-pulled-now
}

# agones_state looks at how far Agones has got. It sets part (of 3), step and
# problem, and ready once Agones runs. An install that keeps failing stops
# the script.
agones_state() {
	part=0 step="下载并安装 Agones 的安装包" problem='' ready=''
	if [ "$(kubectl -n kube-system get job helm-install-agones -o jsonpath='{.status.succeeded}' 2>/dev/null)" != 1 ]; then
		pod=$(kubectl -n kube-system get pods -l job-name=helm-install-agones --sort-by=.metadata.creationTimestamp -o name 2>/dev/null | tail -n 1)
		[ -n "$pod" ] || return 0
		# The job may replace the pod between two calls: a missing pod is no error.
		created=$(kubectl -n kube-system get "$pod" -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null) || return 0
		[ "$(date -d "$created" +%s 2>/dev/null || echo 0)" -ge "$agones_since" ] || return 0
		pull=$(pull_problem kube-system "$pod")
		restarts=$(kubectl -n kube-system get "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null) || return 0
		if [ -n "$pull" ]; then
			problem="下载安装程序出错，K3s 在重试：$pull"
		elif [ "${restarts:-0}" -gt 0 ]; then
			# Each failed attempt is read once.
			if [ "$restarts" != "$read_restarts" ]; then
				read_restarts=$restarts
				error=$({ kubectl -n kube-system logs "$pod" --previous; kubectl -n kube-system logs "$pod"; } 2>/dev/null |
					grep -m 1 '^Error' | cut -c 1-400)
				error=${error:-详情用 k3s kubectl -n kube-system logs $pod 查看}
				case "$error" in
				*lookup*|*"i/o timeout"*|*"dial tcp"*|*"cannot be reached"*)
					[ -n "$network_checked" ] || { network_checked=1; network_check || true; } ;;
				esac
			fi
			[ "$restarts" -lt 3 ] || fail "Agones 的安装包装了 $restarts 次都失败了：$error"
			problem="安装失败 $restarts 次，K3s 在重试：$error"
		fi
		return 0
	fi
	pulled
	part=1 step="下载 Agones 的镜像"
	pods=$(kubectl -n agones-system get pods -l app=agones -o jsonpath='{range .items[*]}{.metadata.name}|{.status.containerStatuses[0].imageID}|{.status.containerStatuses[0].state.waiting.reason}|{.status.containerStatuses[0].state.waiting.message}{"\n"}{end}' 2>/dev/null) || true
	[ -n "$pods" ] || return 0
	echo "$pods" | grep -q '^[^|]*||' || part=2 step="启动 Agones"
	if [ "$(kubectl -n agones-system get deployment agones-controller agones-extensions -o jsonpath='{.items[*].status.readyReplicas}' 2>/dev/null)" = "1 1" ]; then
		part=3 step="Agones 已经在运行" ready=1
		return 0
	fi
	problem=$(echo "$pods" | awk -F'|' '
		$3 ~ /ErrImagePull|ImagePullBackOff/ { print "下载镜像出错，K3s 在重试：" substr($4, 1, 240); exit }
		$3 == "CrashLoopBackOff" { sub(/-[^-]*-[^-]*$/, "", $1); print $1 " 启动后退出了，K3s 在重试"; exit }')
}

say "安装 Agones $AGONES_VERSION"
status agones 0 3 "下载并安装 Agones 的安装包" ""
# A rerun starts a fresh attempt at once instead of waiting out the back-off
# of attempts that failed, maybe for a reason fixed since.
if [ "$(kubectl -n kube-system get job helm-install-agones -o jsonpath='{.status.succeeded}' 2>/dev/null)" != 1 ] &&
	[ -n "$(kubectl -n kube-system get pods -l job-name=helm-install-agones -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' 2>/dev/null | awk '$1 > 0')" ]; then
	kubectl -n kube-system delete job helm-install-agones >/dev/null 2>&1 || true
	agones_since=$(date +%s)
fi
read_restarts='' network_checked='' error=''
deadline=$(($(date +%s) + 900))
while :; do
	agones_state
	status agones "$part" 3 "$step" "$problem"
	[ -z "$ready" ] || break
	[ "$(date +%s)" -lt "$deadline" ] || fail "Agones 15 分钟还没装好，停在“$step”${problem:+：$problem}"
	sleep 5
done
rm -f /tmp/gamefleet-pulled /tmp/gamefleet-pulled-now
token_ready() { [ -n "$(kubectl -n "$NAMESPACE" get secret gamefleet-token -o jsonpath='{.data.token}')" ]; }
wait_for " GameFleet 的账号" token_ready

token=$(kubectl -n "$NAMESPACE" get secret gamefleet-token -o jsonpath='{.data.token}' | base64 -d)
ca=$(kubectl -n "$NAMESPACE" get secret gamefleet-token -o jsonpath='{.data.ca\.crt}')
body=$(printf '{"stage":"connect","connection":{"name":"%s","region":"%s","server":"https://%s:6443","caBase64":"%s","token":"%s","namespace":"%s","joinUrl":"https://%s:6443","joinToken":"%s"}}' \
	"$name" "$region" "$public_ip" "$ca" "$token" "$NAMESPACE" "$public_ip" "$agent_token")

say "K3s 和 Agones 已经装好，GameFleet 正在连接这个集群"
while :; do
	status=$(report "$body")
	case "$status" in
	200) break ;;
	403) fail "加入码过期了；请在控制台重新生成，再执行新的命令（已经装好的部分会接着用）" ;;
	esac
	say "还没连上：$(cat /tmp/gamefleet-report 2>/dev/null)"
	say "请确认云防火墙对 GameFleet 所在的机器放行了 TCP 6443；15 秒后再试"
	sleep 15
done
rm -f /tmp/gamefleet-report

if [ -n "$control_plane_only" ]; then
	cat <<EOF

GameFleet：集群 $name 已经接入。这台机器只做控制面，不跑战斗服：到控制台的集群页加机器来跑战斗服。
EOF
else
	cat <<EOF

GameFleet：集群 $name 已经接入，可以在控制台里打开它了。
玩家通过 UDP $MIN_PORT-$MAX_PORT 连接战斗服：云防火墙要对所有地址放行这段端口。
EOF
fi
