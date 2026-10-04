#!/usr/bin/env bash
# batch-p2.sh — 全池 Sequential_Tasks_2 accept + claim 批量驱动
# 用法: bash batch-p2.sh [concurrency]
# 并发默认 4（jeff 偏好）
# 输出: logs/batch-p2-<ts>/  每号一个日志 + summary.tsv
# 幂等: 已 claimed 的号脚本会返回 already_claimed，可重跑

set -u
cd "$(dirname "$0")/.."

TS=$(date +%Y%m%d-%H%M%S)
LOGDIR="logs/batch-p2-$TS"
mkdir -p "$LOGDIR"

CONCURRENCY=${1:-4}
SUMMARY="$LOGDIR/summary.tsv"
: > "$SUMMARY"
echo -e "uid\tstatus\tdetail" >> "$SUMMARY"

# 收集所有账号 uid
UIDS=()
while IFS= read -r f; do
  u=$(basename "$f" .json | sed 's/^workbuddy-//')
  UIDS+=("$u")
done < <(ls auths/workbuddy-*.json 2>/dev/null)

TOTAL=${#UIDS[@]}
echo "[batch-p2] total=$TOTAL concurrency=$CONCURRENCY logdir=$LOGDIR"

run_one() {
  local uid="$1"
  local log="$LOGDIR/$uid.log"
  # task_runner.py 需要 python3.11（task_common 用了 dict | None 语法）
  local py
  py=$(command -v python3.11 || command -v python3)
  # task_runner 内部已做 P1 prereq 门控，P1 未 claimed 会 skip
  "$py" scripts/task_runner.py "$uid" --yes --only Sequential_Tasks_2 >"$log" 2>&1
  local rc=$?
  # 提取关键行
  local status="unknown" detail=""
  if grep -q "claim 200 ok" "$log"; then
    status="claimed"
    detail=$(grep "claim 200 ok" "$log" | tail -1 | sed 's/.*claim 200 ok//')
  elif grep -q "claim 200 already_claimed" "$log"; then
    status="already"
  elif grep -q "P1 未解锁" "$log"; then
    status="skipped_p1"
  elif grep -q "mp 口径任务不存在" "$log"; then
    status="skipped_no_p1"
  elif grep -qE "claim [0-9]+ task not completed" "$log"; then
    status="not_completed"
  elif [ $rc -ne 0 ]; then
    status="error_rc$rc"
  else
    status="unknown"
    detail=$(tail -3 "$log" | tr '\n' ' ' | head -c 200)
  fi
  printf "%s\t%s\t%s\n" "$uid" "$status" "$detail" >> "$SUMMARY"
  echo "[batch-p2] ${uid:0:8} -> $status"
}

export -f run_one
export LOGDIR SUMMARY

# 并发 4 跑（用 xargs -P）
printf '%s\n' "${UIDS[@]}" | xargs -P "$CONCURRENCY" -I {} bash -c 'run_one "$@"' _ {}

echo ""
echo "[batch-p2] done. summary:"
echo "-------- status counts --------"
awk -F'\t' 'NR>1 {c[$2]++} END {for (k in c) print k, c[k]}' "$SUMMARY" | sort
echo "-------- summary file --------"
echo "$SUMMARY"
