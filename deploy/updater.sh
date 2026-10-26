#!/bin/sh
# GameFleet's updater, run by the systemd service gamefleet-updater that
# install.sh sets up. GameFleet asks for an update by writing
# update/request.json; this script downloads the release's image and puts it
# in place with install.sh. GameFleet itself has no hold on Docker.
#
# A request names a version, never an image or a command: the image is
# GAMEFLEET_IMAGE_REPO from .env with the version as its tag, so only
# GameFleet releases are ever installed. Every message this script writes is
# one of its own, so status.json needs no escaping.
set -u
cd "$(dirname "$0")"
dir=update
action='' version='' backup=''

status() {
	printf '{"action":"%s","version":"%s","state":"%s","error":"%s","at":"%s"}\n' \
		"$action" "$version" "$1" "${2:-}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$dir/.status.json"
	chmod 644 "$dir/.status.json"
	mv -f "$dir/.status.json" "$dir/status.json"
}

setting() { sed -n "s/^$1=//p" .env | tail -n 1; }

# deploy_files IMAGE puts the deploy files that IMAGE carries in /deploy next
# to this script. Each file is renamed into place, so this script, which is
# among them, keeps running from the copy it started with.
deploy_files() {
	rm -rf "$dir/deploy"
	cid=$(docker create "$1" 2>/dev/null) || return 1
	docker cp "$cid:/deploy/." "$dir/deploy" >/dev/null 2>&1
	copied=$?
	docker rm "$cid" >/dev/null 2>&1
	[ $copied = 0 ] || return 1
	for f in "$dir"/deploy/* "$dir"/deploy/.env.example; do
		[ -f "$f" ] && mv -f "$f" .
	done
	rm -rf "$dir/deploy"
}

download() {
	status downloading
	: >"$dir/pull.jsonl"
	chmod 644 "$dir/pull.jsonl"
	# Docker's API streams the pull as JSON lines; GameFleet adds them up.
	if ! curl -sS --max-time 1800 --unix-socket /var/run/docker.sock -X POST \
		"http://docker/images/create?fromImage=$repo&tag=$version" >>"$dir/pull.jsonl" 2>/dev/null; then
		status failed "连不上这台机器上的 Docker"
		return
	fi
	if grep -q '"error"' "$dir/pull.jsonl" || ! docker image inspect "$image" >/dev/null 2>&1; then
		status failed "镜像没下载下来，原因在管理机的 /opt/gamefleet/update/pull.jsonl 里"
		return
	fi
	status downloaded
}

apply() {
	if ! docker image inspect "$image" >/dev/null 2>&1; then
		status failed "这个版本还没下载"
		return
	fi
	status applying
	previous=$(setting GAMEFLEET_IMAGE)
	domain=$(setting GAMEFLEET_DOMAIN)
	email=$(setting ACME_EMAIL)
	if ! deploy_files "$image"; then
		status failed "新版本的镜像里没有部署文件"
		return
	fi
	if GAMEFLEET_UPDATER=1 sh install.sh --domain "$domain" --email "$email" --image "$image" >"$dir/apply.log" 2>&1; then
		status applied
		return
	fi
	# Back to the version that ran before, with its own deploy files when it
	# carries them.
	deploy_files "$previous" || true
	if GAMEFLEET_UPDATER=1 sh install.sh --domain "$domain" --email "$email" --image "$previous" >>"$dir/apply.log" 2>&1; then
		status failed "新版本没有启动起来，已经回到原来的版本。经过见管理机的 /opt/gamefleet/update/apply.log"
		return
	fi
	# The new version may have changed the database, which the old one then
	# refuses: it needs the backup made before the update.
	restore='按安装文档“备份和恢复”换回更新前的数据库，再用原来的版本运行安装脚本'
	[ -z "$backup" ] || restore="${restore}。更新前的数据库备份是 GameFleet 数据卷里的 backups/${backup}"
	status failed "新版本没有启动起来，换回原来的版本也没有成功，GameFleet 现在可能没在运行。${restore}。经过见管理机的 /opt/gamefleet/update/apply.log"
}

repo=$(setting GAMEFLEET_IMAGE_REPO)
[ -n "$repo" ] || repo=ghcr.io/xuhuanhello/gamefleet
mkdir -p "$dir"

# A download or an update that was under way when this script stopped, with
# the machine restarting say, did not finish; GameFleet would wait for it for
# ever. Say so, and take requests again.
if [ -f "$dir/status.json" ]; then
	state=$(sed -n 's/.*"state":"\([a-z]*\)".*/\1/p' "$dir/status.json")
	if [ "$state" = downloading ] || [ "$state" = applying ]; then
		action=$(sed -n 's/.*"action":"\([a-z]*\)".*/\1/p' "$dir/status.json")
		version=$(sed -n 's/.*"version":"\([0-9.]*\)".*/\1/p' "$dir/status.json")
		status failed "更新器做到一半停了（比如管理机重启了），这次没有做完，可以再试一次"
	fi
fi

while :; do
	if [ -f "$dir/request.json" ]; then
		action=$(sed -n 's/.*"action": *"\([a-z]*\)".*/\1/p' "$dir/request.json")
		version=$(sed -n 's/.*"version": *"\([0-9.]*\)".*/\1/p' "$dir/request.json")
		backup=$(sed -n 's/.*"backup": *"\([A-Za-z0-9._-]*\)".*/\1/p' "$dir/request.json")
		image="$repo:$version"
		# Say busy before the request goes, so that GameFleet, which counts a
		# waiting request as busy too, never sees the updater idle in between.
		if ! echo "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
			status failed "请求里的版本号不对"
			action=''
		elif [ "$action" = download ]; then
			status downloading
		elif [ "$action" = apply ]; then
			status applying
		else
			status failed "不认识的请求"
			action=''
		fi
		rm -f "$dir/request.json"
		if [ "$action" = download ]; then
			download
		elif [ "$action" = apply ]; then
			apply
			# systemd starts this script again, from the version just put in place.
			exit 0
		fi
	fi
	sleep 2
done
