#!/usr/bin/env bash
# 汇总 nohup.out / 日志中所有 sse_summary 的信号字段分布。
# 用法: bash tools/sse_signals_summary.sh
LOG="${1:-nohup.out}"
echo "=== sse_summary 总数: $(grep -c sse_summary "$LOG") ==="
echo
echo "=== 信号位组合分布（patch/asst/img/tool/delta） ==="
grep "sse_summary" "$LOG" | python3 -c '
import sys, json, re
from collections import Counter
combos = Counter()
frames_by_combo = {}
for line in sys.stdin:
    m = re.search(r"\{.*\}", line)
    if not m: continue
    try: d = json.loads(m.group())
    except: continue
    key = "p={} a={} i={} t={} d={}".format(
        int(d.get("saw_patch", False)),
        int(d.get("saw_assistant", False)),
        int(d.get("saw_image_gen", False)),
        int(d.get("saw_tool", False)),
        int(d.get("saw_delta", False)),
    )
    combos[key] += 1
    frames_by_combo.setdefault(key, []).append(d.get("frames", 0))
for k, v in combos.most_common():
    fs = frames_by_combo[k]
    avg = sum(fs)/len(fs) if fs else 0
    print(f"  {v:>4}x  {k}  | frames avg={avg:.1f}, range={min(fs)}-{max(fs)}")
'
echo
echo "=== 最近 5 条失败请求关联（status_code != 200）==="
psql -h 127.0.0.1 -U postgres -d sub2api -c "SELECT id, request_path, status_code, substring(error_message,1,80), created_at FROM ops_error_logs WHERE status_code != 200 AND created_at > now() - interval '2 hours' ORDER BY id DESC LIMIT 5;" 2>/dev/null || echo "(psql 不可用，跳过)"
