#!/bin/zsh

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd)
cd "$script_dir"

export PATH="/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"

load_env_file() {
	local env_file="$1"
	if [[ -f "$env_file" ]]; then
		set -a
		source "$env_file"
		set +a
	fi
}

load_env_file "$script_dir/update.env"
load_env_file "$script_dir/.env"
load_env_file "$HOME/.config/dircast/update.env"

: "${DROPBOX_REFRESH_TOKEN:?DROPBOX_REFRESH_TOKEN must be set}"
: "${DROPBOX_APP_KEY:?DROPBOX_APP_KEY must be set}"
: "${DROPBOX_APP_SECRET:?DROPBOX_APP_SECRET must be set}"

: "${DIRCAST_DROPBOX_PATH:=/dircast/}"
: "${DIRCAST_BASE_URL:=https://podcasts.justcast.com/reilly}"
: "${DIRCAST_IMAGE_URL:=https://justcast.sfo2.digitaloceanspaces.com/js-production/1647481331315-reilly%20copy.jpeg}"
: "${DIRCAST_REMOTE:=origin}"
: "${DIRCAST_BRANCH:=main}"
: "${GO_BIN:=/usr/local/go/bin/go}"
: "${DIRCAST_UPDATE_TIMEOUT_SECONDS:=900}"

if ! [[ "$DIRCAST_UPDATE_TIMEOUT_SECONDS" =~ '^[0-9]+$' ]] || (( DIRCAST_UPDATE_TIMEOUT_SECONDS <= 0 )); then
	echo "DIRCAST_UPDATE_TIMEOUT_SECONDS must be a positive integer number of seconds."
	exit 1
fi

lock_dir="${TMPDIR:-/tmp}/dircast-update.lock"
if ! mkdir "$lock_dir" 2>/dev/null; then
	if [[ -f "$lock_dir/pid" ]]; then
		stale_pid=$(<"$lock_dir/pid")
		if ! kill -0 "$stale_pid" 2>/dev/null; then
			rm -rf "$lock_dir"
			mkdir "$lock_dir"
		else
			echo "Another dircast update is already running; exiting."
			exit 0
		fi
	else
		echo "Another dircast update is already running; exiting."
		exit 0
	fi
fi
print -r -- "$$" > "$lock_dir/pid"

tmp_feed=""
timeout_marker=""
run_pid=""
watchdog_pid=""

terminate_process_tree() {
	local pid="$1"
	local children=()

	children=($(pgrep -P "$pid" 2>/dev/null || true))
	if (( ${#children[@]} > 0 )); then
		kill -TERM "${children[@]}" 2>/dev/null || true
	fi
	kill -TERM "$pid" 2>/dev/null || true

	sleep 5

	children=($(pgrep -P "$pid" 2>/dev/null || true))
	if (( ${#children[@]} > 0 )); then
		kill -KILL "${children[@]}" 2>/dev/null || true
	fi
	kill -KILL "$pid" 2>/dev/null || true
}

cleanup() {
	if [[ -n "$watchdog_pid" ]]; then
		kill "$watchdog_pid" 2>/dev/null || true
	fi
	if [[ -n "$run_pid" ]]; then
		terminate_process_tree "$run_pid"
	fi
	rm -rf "$lock_dir"
	if [[ -n "$timeout_marker" && -f "$timeout_marker" ]]; then
		rm -f "$timeout_marker"
	fi
	if [[ -n "$tmp_feed" && -f "$tmp_feed" ]]; then
		rm -f "$tmp_feed"
	fi
}
trap cleanup EXIT INT TERM

run_with_timeout() {
	local timeout_seconds="$1"
	shift
	local exit_status=0

	"$@" > "$tmp_feed" &
	run_pid=$!
	(
		sleep "$timeout_seconds"
		if kill -0 "$run_pid" 2>/dev/null; then
			print -r -- "timeout" > "$timeout_marker"
			echo "dircast update timed out after ${timeout_seconds}s; terminating go run."
			terminate_process_tree "$run_pid"
		fi
	) &
	watchdog_pid=$!

	wait "$run_pid" || exit_status=$?
	run_pid=""
	kill "$watchdog_pid" 2>/dev/null || true
	wait "$watchdog_pid" 2>/dev/null || true
	watchdog_pid=""

	if [[ -f "$timeout_marker" ]]; then
		return 124
	fi
	return "$exit_status"
}

if ! command -v "$GO_BIN" >/dev/null 2>&1; then
	echo "Go binary not found: $GO_BIN"
	exit 1
fi

tmp_feed=$(mktemp "${TMPDIR:-/tmp}/dircast-feed.XXXXXX")
timeout_marker="${tmp_feed}.timeout"

run_with_timeout "$DIRCAST_UPDATE_TIMEOUT_SECONDS" "$GO_BIN" run main.go "$DIRCAST_DROPBOX_PATH" "$DIRCAST_BASE_URL" "$DIRCAST_IMAGE_URL"

if [[ -f feed.rss ]] && cmp -s "$tmp_feed" feed.rss; then
	echo "feed.rss not modified."
	exit 0
fi

mv "$tmp_feed" feed.rss
tmp_feed=""

git add feed.rss
git commit -m "Update feed.rss"
git push "$DIRCAST_REMOTE" "$DIRCAST_BRANCH"
