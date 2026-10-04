#!/usr/bin/env bash
# new-account-claim.sh — 新号领积分一键流：签到 → 活跃上报+连登+领猫 → 成长任务全量领取
#
# 用法:
#   ./scripts/new-account-claim.sh --pool=hk --uid=ead6e63f
#
#   --pool=phone|qq|hk   号池（默认 phone）
#   --uid=<前缀>         新号 uid 前 8 位（必填，单号模式）
#
# 三个环节全部幂等，重复跑只会「已领跳过」，不会重复入账：
#   1) 签到             POST /admin/api/checkin（全池幂等，ALREADY=今天已签）
#   2) activity_bin     /app/activity_bin <uid>（5连发活跃上报 + streak 自检 + 领猫 + 连登奖励/抽奖）
#   3) task_runner      python3 scripts/task_runner.py <uid> --yes（23 个成长任务 accept→点亮→claim）
#
# 不覆盖的自动项（由容器 scheduler 按排程跑，无需手动）：
#   - 连登奖励：streak 累计 7/14/28 天自动领档位（claimGrowthRewards）
#   - 夜猫任务 black_cat：仅 23:00-08:00 CST 窗口内可领
#   - 猫猫旅行：次日 09:00 travel_hours 自动派出→到站→领奖
set -euo pipefail

cd "$(dirname "$0")/.."

# ── 参数解析 ──
POOL="phone"
UIDP=""
for a in "$@"; do
    case "$a" in
        --pool=*) POOL="${a#--pool=}" ;;
        --uid=*)  UIDP="${a#--uid=}" ;;
        --help|-h) sed -n '2,20p' "$0"; exit 0 ;;
    esac
done

# ── 号池 → 容器/端口/配置 ──
case "$POOL" in
    phone) CONTAINER="workbuddy2api";    PORT=7863; CFG="config.json" ;;
    qq)    CONTAINER="workbuddy2api-qq"; PORT=7865; CFG="config-qq.json" ;;
    hk)    CONTAINER="workbuddy2api-hk"; PORT=7866; CFG="config-hk.json" ;;
    *) echo "未知号池: $POOL（want phone|qq|hk）" >&2; exit 2 ;;
esac

[[ -n "$UIDP" ]] || { echo "缺少 --uid=<新号 uid 前缀>（前 8 位）" >&2; exit 2; }

KEY=$(python3 -c "import json;print(json.load(open('$CFG'))['api_key'])")
docker ps --format '{{.Names}}' | grep -qx "$CONTAINER" || { echo "容器 $CONTAINER 未运行" >&2; exit 1; }

echo "== 号池 $POOL / 容器 $CONTAINER / :$PORT / uid 前缀 $UIDP =="

# ── 1) 签到（全池幂等）──
echo ""
echo "[1/3] 每日签到"
curl -s -X POST -H "Authorization: Bearer $KEY" "http://127.0.0.1:$PORT/admin/api/checkin" \
    | python3 -c 'import json,sys
d=json.load(sys.stdin)
for r in d.get("results",[]):
    st = "ok remain=%s" % r.get("remain") if r.get("success") else "fail: %s" % r
    print("  %s  %s" % (r.get("uid","?")[:8], st))
' 2>/dev/null || echo "  签到调用失败（继续后续环节）"

# ── 2) activity（5连发 + streak + 领猫 + 连登奖励/抽奖）──
echo ""
echo "[2/3] 活跃上报 + 连登 + 领猫（activity_bin 单号）"
docker exec -w /app "$CONTAINER" /app/activity_bin "$UIDP" 2>&1 | sed 's/^/  /'

# ── 3) task_runner 全量成长任务 ──
echo ""
echo "[3/3] 成长任务全量领取（task_runner --yes，约 1-2 分钟）"
docker exec -w /app "$CONTAINER" python3 scripts/task_runner.py "$UIDP" --yes 2>&1 | sed 's/^/  /'

# ── 最终余额 ──
echo ""
echo "== 最终余额 =="
curl -s "http://127.0.0.1:$PORT/admin/api/credits" \
    | python3 -c 'import json,sys
d=json.load(sys.stdin)
for a in d.get("accounts",[]):
    print("  %s (%s)  remain=%s" % (a.get("uid","?")[:8], a.get("nickname","?"), a.get("remain")))
' 2>/dev/null

echo ""
echo "完成。连登奖励累计 7/14/28 天自动领；夜猫 black_cat 仅 23:00-08:00 CST 窗口可领；猫猫旅行次日 09:00 自动派出。"
