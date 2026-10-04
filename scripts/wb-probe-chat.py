#!/usr/bin/env python3
"""wb 号池真实探活：直接打上游 copilot.tencent.com /v2/chat/completions 发一条真实消息。

用法: python3 wb-probe-chat.py <auth_dir>
判读: ALIVE=拿到正常 chat 响应; DEAD=401/403/token 失效; RISKY=余额不足/限流(号本身活着)
"""
import hashlib
import json
import sys
import urllib.request
import urllib.error
import glob
import os

CHAT_URL = "https://copilot.tencent.com/v2/chat/completions"
UA = "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1"


def derive(uid: str, purpose: str) -> str:
    return hashlib.sha256(f"wb2a:{purpose}:{uid}".encode()).hexdigest()[:36]


def probe(auth_file: str) -> dict:
    d = json.load(open(auth_file))
    acc, auth = d.get("account", {}), d.get("auth", {})
    uid = acc.get("uid", "")
    nick = acc.get("nickname", "?")
    at = auth.get("accessToken", "")
    domain = auth.get("domain", "www.codebuddy.cn")
    ent = acc.get("enterpriseId", "")

    body = json.dumps({
        "model": "fast-model",
        "messages": [{"role": "user", "content": "只回复两个字：正常"}],
        "stream": True,
    }).encode()

    req = urllib.request.Request(CHAT_URL, data=body, method="POST")
    h = req.headers
    h["Content-Type"] = "application/json"
    h["Accept"] = "application/json, text/event-stream"
    h["X-Requested-With"] = "XMLHttpRequest"
    h["Origin"] = "https://www.codebuddy.cn"
    h["Referer"] = "https://www.codebuddy.cn/"
    h["User-Agent"] = UA
    h["X-CodeBuddy-Request"] = "1"
    h["Accept-Language"] = "zh-CN"
    h["Authorization"] = f"Bearer {at}"
    h["X-User-Id"] = uid
    h["X-Enterprise-Id" if ent else "X-No-Enterprise-Id"] = ent or "1"
    h["X-Domain" if domain else "X-No-Department-Info"] = domain or "1"
    h["X-Machine-ID"] = derive(uid, "machine")
    h["X-Session-ID"] = derive(uid, "session")

    try:
        with urllib.request.urlopen(req, timeout=45) as r:
            raw = r.read().decode(errors="replace")
            # 解析 SSE: data: {...} 行, 聚合 delta.content
            chunks, has_data = [], False
            for line in raw.splitlines():
                line = line.strip()
                if not line.startswith("data:"):
                    continue
                payload = line[5:].strip()
                if payload == "[DONE]":
                    break
                has_data = True
                try:
                    j = json.loads(payload)
                    for ch in j.get("choices") or []:
                        dc = (ch.get("delta") or {}).get("content")
                        if dc:
                            chunks.append(dc)
                except json.JSONDecodeError:
                    pass
            if chunks:
                return {"uid": uid, "nick": nick, "status": "ALIVE",
                        "detail": f"http {r.status}, 回复: {''.join(chunks)[:40]!r}"}
            if has_data:
                return {"uid": uid, "nick": nick, "status": "ALIVE",
                        "detail": f"http {r.status}, SSE 有帧但无文本 {raw[:80]!r}"}
            return {"uid": uid, "nick": nick, "status": "RISKY",
                    "detail": f"http {r.status} 但非 SSE: {raw[:100]!r}"}
    except urllib.error.HTTPError as e:
        raw = e.read().decode(errors="replace")[:200]
        status = "DEAD" if e.code in (401, 403) else "RISKY"
        return {"uid": uid, "nick": nick, "status": status,
                "detail": f"http {e.code}: {raw[:120]}"}
    except Exception as e:
        return {"uid": uid, "nick": nick, "status": "ERROR", "detail": f"{type(e).__name__}: {e}"}


def main():
    auth_dir = sys.argv[1]
    files = sorted(glob.glob(os.path.join(auth_dir, "workbuddy-*.json")))
    print(f"探活目录 {auth_dir}: {len(files)} 个号, 真实发消息到 {CHAT_URL}\n")
    counts = {"ALIVE": 0, "DEAD": 0, "RISKY": 0, "ERROR": 0}
    results = []
    for f in files:
        r = probe(f)
        counts[r["status"]] = counts.get(r["status"], 0) + 1
        results.append(r)
        mark = {"ALIVE": "✓", "DEAD": "✗死", "RISKY": "⚠", "ERROR": "?"}.get(r["status"], "?")
        print(f"  {mark} {r['uid'][:8]}  {r['nick']:<14}  {r['detail']}")
    print(f"\n汇总: ALIVE {counts['ALIVE']} / DEAD {counts['DEAD']} / RISKY {counts['RISKY']} / ERROR {counts['ERROR']}  (共 {len(files)})")
    # 死号清单单独列
    dead = [r for r in results if r["status"] == "DEAD"]
    if dead:
        print("死号:", ", ".join(f"{r['uid'][:8]}({r['nick']})" for r in dead))


if __name__ == "__main__":
    main()
