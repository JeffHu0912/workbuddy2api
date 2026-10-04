#!/usr/bin/env bash
# batch-p1-then-p2.sh — 对指定 uid 列表先跑 P1 再跑 P2（链式）
# 用法: bash batch-p1-then-p2.sh uid1 uid2 ...
# 串行（列表很短，且每个号内部 P1 → P2 必须串行等 P1 claim 完才能 accept P2）

set -u
cd "$(dirname "$0")/.."

TS=$(date +%Y%m%d-%H%M%S)
LOGDIR="logs/batch-p1-p2-$TS"
mkdir -p "$LOGDIR"
SUMMARY="$LOGDIR/summary.tsv"
: > "$SUMMARY"
echo -e "uid\tp1_status\tp2_status\ttotal_credit" >> "$SUMMARY"

PY=$(command -v python3.11 || command -v python3)

extract_status() {
  local log="$1" task="$2"
  if grep -q "$task: claim 200 ok" "$log"; then
    grep "$task: claim 200 ok" "$log" | tail -1 | grep -oE 'credit=\+[0-9]+ energy=\+[0-9]+'
  elif grep -q "$task.*已领，跳过" "$log"; then
    echo "already"
  elif grep -q "$task.*P1 未解锁" "$log"; then
    echo "skipped_p1"
  elif grep -q "$task.*mp 口径任务不存在" "$log"; then
    echo "skipped_no_p1"
  else
    echo "unknown"
  fi
}

extract_credit() {
  local log="$1"
  # 从 task_runner done 行提取 credit=+N
  grep "task_runner done" "$log" | tail -1 | grep -oE 'credit=\+[0-9]+' | head -1 | grep -oE '[0-9]+' || echo "0"
}

for uid in "$@"; do
  echo "[batch-p1-p2] === $uid ==="
  log="$LOGDIR/$uid.log"
  : > "$log"

  # P1
  "$PY" scripts/task_runner.py "$uid" --yes --only Sequential_Tasks_1 >>"$log" 2>&1
  p1_status=$(extract_status "$log" "Sequential_Tasks_1")
  p1_credit=$(extract_credit "$log")
  echo "[batch-p1-p2] ${uid:0:8} P1 -> $p1_status (+${p1_credit}c)"

  # P1 完成后才能跑 P2（链式）
  sleep 2
  p2_status="not_attempted"
  p2_credit="0"
  if [ "$p1_status" != "skipped_p1" ] && [ "$p1_status" != "unknown" ]; then
    "$PY" scripts/task_runner.py "$uid" --yes --only Sequential_Tasks_2 >>"$log" 2>&1
    p2_status=$(extract_status "$log" "Sequential_Tasks_2")
    # 算 P2 的 credit：从该次调用的 done 行
    p2_credit=$(grep "task_runner done" "$log" | tail -1 | grep -oE 'credit=\+[0-9]+' | head -1 | grep -oE '[0-9]+' || echo "0")
    echo "[batch-p1-p2] ${uid:0:8} P2 -> $p2_status (+${p2_credit}c)"
  else
    echo "[batch-p1-p2] ${uid:0:8} P2 skipped (P1 status=$p1_status)"
  fi

  total=$((p1_credit + p2_credit))
  printf "%s\t%s\t%s\t+%dc\n" "$uid" "$p1_status" "$p2_status" "$total" >> "$SUMMARY"
done

echo ""
echo "[batch-p1-p2] done. summary:"
cat "$SUMMARY"
echo ""
echo "-------- total credit --------"
awk -F'\t' 'NR>1 {gsub(/[+c]/,"",$4); s+=$4} END {print "+"s"c"}' "$SUMMARY"
