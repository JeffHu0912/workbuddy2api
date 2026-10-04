#!/usr/bin/env bash
# watch-new-accounts.sh — 监听 auths*/ 新增 workbuddy-*.json，自动跑 签到 + task_runner
#
# 触发场景：
#   - login.sh / auto-add-account.sh 落盘新凭证
#   - 手工拷贝凭证进池
#
# 动作（按号池）：
#   1) docker restart workbuddy2api[-qq|-hk]   （手工落盘不热加载，必须重启）
#   2) 等容器 healthy（最多 30s）
#   3) POST /admin/api/checkin                  （新号首次落地 remain=0 → 签到后 +2100）
#   4) python3.11 scripts/task_runner.py <uid8> --yes（WB2A_AUTHS 指向对应 auths 目录）
#
# 用法：
#   ./scripts/watch-new-accounts.sh            # 前台跑（Ctrl-C 停止）
#   nohup ./scripts/watch-new-accounts.sh >logs/watch-new-accounts.log 2>&1 &
#   或装成 launchd（见下方 plist 模板注释）
#
# 依赖：fswatch（brew install fswatch）、docker、python3.11、curl
#
# launchd plist 模板（~/Library/LaunchAgents/com.jeff.wb2api-watch-new.plist）：
#   <?xml version="1.0" encoding="UTF-8"?>
#   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
#   <plist version="1.0"><dict>
#     <key>Label</key><string>com.jeff.wb2api-watch-new</string>
#     <key>ProgramArguments</key><array>
#       <string>/Users/jeff/project/workbuddy2api/scripts/watch-new-accounts.sh</string>
#     </array>
#     <key>RunAtLoad</key><true/>
#     <key>KeepAlive</key><true/>
#     <key>StandardOutPath</key><string>/Users/jeff/project/workbuddy2api/logs/watch-new-accounts.log</string>
#     <key>StandardErrorPath</key><string>/Users/jeff/project/workbuddy2api/logs/watch-new-accounts.log</string>
#   </dict></plist>
#   launchctl load ~/Library/LaunchAgents/com.jeff.wb2api-watch-new.plist

set -u
cd "$(dirname "$0")/.."
ROOT=$PWD
LOG_DIR="$ROOT/logs"
mkdir -p "$LOG_DIR"

PY=/opt/homebrew/bin/python3.11
TASK_RUNNER="$ROOT/scripts/task_runner.py"
STATE_DIR="$ROOT/data/.watch-new"
mkdir -p "$STATE_DIR"

log() { printf '[%s] %s\n' "$(date '+%F %T')" "$*"; }

command -v fswatch >/dev/null || { log "ERR: fswatch 未安装（brew install fswatch）"; exit 1; }
[ -x "$PY" ] || { log "ERR: $PY 不存在"; exit 1; }

# 已处理过的 uid 落盘标记（避免重启 watcher 后重复跑）
mark_done() { touch "$STATE_DIR/$1.done"; }
is_done()   { [ -f "$STATE_DIR/$1.done" ]; }

process_one() {
    local file=$1
    local base=$(basename "$file")
    # 只处理 workbuddy-<uid>.json
    [[ "$base" =~ ^workbuddy-([a-f0-9-]{36})\.json$ ]] || return 0
    local uid="${BASH_REMATCH[1]}"
    local uid8="${uid:0:8}"

    # 判号池：按所在目录
    local pool dir container cfg port
    case "$file" in
        */auths-hk/*)  pool=hk;    dir=auths-hk;  container=workbuddy2api-hk; cfg=config-hk.json; port=7866 ;;
        */auths-qq/*)  pool=qq;    dir=auths-qq;  container=workbuddy2api-qq; cfg=config-qq.json; port=7865 ;;
        */auths/*)     pool=phone; dir=auths;     container=workbuddy2api;    cfg=config.json;    port=7863 ;;
        *) return 0 ;;
    esac

    if is_done "$uid"; then
        log "skip $uid8 ($pool) — 已处理过"
        return 0
    fi

    log "new account $uid8 ($pool) → restart $container"
    docker restart "$container" >/dev/null 2>&1 || { log "ERR: docker restart $container 失败"; return 1; }

    # 等 healthy（最多 30s）
    local ok=0
    for i in $(seq 1 30); do
        local st
        st=$(docker inspect --format '{{.State.Health.Status}}' "$container" 2>/dev/null || echo unknown)
        if [ "$st" = "healthy" ]; then ok=1; break; fi
        sleep 1
    done
    [ "$ok" = "1" ] || log "WARN: $container 30s 内未 healthy，继续（可能仍在 starting）"

    # 签到（新号 remain=0 → +2100）
    local key
    key=$(python3 -c "import json;print(json.load(open('$cfg'))['api_key'])" 2>/dev/null)
    if [ -n "$key" ]; then
        log "checkin $uid8 ($pool)"
        curl -s -m 60 -X POST -H "Authorization: Bearer $key" \
            "http://127.0.0.1:$port/admin/api/checkin" >/dev/null || log "WARN: checkin 调用失败"
    fi

    # 领积分任务（走 new-account-claim.sh 三步流：checkin → activity_bin → task_runner）
    # 单独的 task_runner 会让 13 个常规任务点亮失败（activity_bin 先激活账号才能点亮）
    if [ -f "$ROOT/scripts/new-account-claim.sh" ]; then
        log "new-account-claim $uid8 ($pool) start"
        "$ROOT/scripts/new-account-claim.sh" --pool="$pool" --uid="$uid8" 2>&1 | \
            grep -E 'task_runner done|claim 200 ok|remain=|完成' | \
            sed "s/^/  /" | while IFS= read -r line; do log "$line"; done
        log "new-account-claim $uid8 done"
    elif [ -f "$TASK_RUNNER" ]; then
        log "task_runner $uid8 ($pool) start (fallback: no new-account-claim.sh)"
        WB2A_AUTHS="$ROOT/$dir" "$PY" "$TASK_RUNNER" "$uid8" --yes 2>&1 | \
            grep -E 'claim 200 ok|task_runner done|ERR|skip' | \
            sed "s/^/  /" | while IFS= read -r line; do log "$line"; done
        log "task_runner $uid8 done"
    fi

    mark_done "$uid"
    log "✅ $uid8 ($pool) 全流程完成"
}

log "watching: $ROOT/auths $ROOT/auths-qq $ROOT/auths-hk (仅追新事件，不扫存量)"

# fswatch 持续监听（只追 Created/Updated 事件，过滤 .done 等噪音）
# 注意：不做启动扫描——避免 watcher 重启时对存量老号重复跑任务/重启容器。
# 若需补处理某个已存在的号，删除 data/.watch-new/<uid>.done 后 touch 对应 json 触发事件。
fswatch -0 --event Created --event Updated \
    "$ROOT/auths" "$ROOT/auths-qq" "$ROOT/auths-hk" 2>/dev/null | \
while IFS= read -r -d '' f; do
    case "$f" in
        */workbuddy-*.json) process_one "$f" ;;
    esac
done
