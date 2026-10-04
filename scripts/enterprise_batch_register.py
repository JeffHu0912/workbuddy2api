#!/usr/bin/env python3
"""
CodeBuddy 企业版批量产号脚本（两条官方 API 通道）

路线 A: CodeBuddy 企业 OpenAPI 批量添加成员（专享版/私有化，绕开手机号）
   POST {base}/api/v1/enterprises/{enterpriseId}/openapi/members/add
   鉴权: Authorization: Bearer pt_xxx（企业级应用 API Key）
   单次 ≤100 个，email 必填，grantLicense:true 直接发 2000 积分

路线 B: OneID 通讯录 API 创建用户（SaaS 旗舰版走这条，仍需手机号）
   POST /openapi/v3/contacts/users
   鉴权: Authorization: Bearer {access_token}（应用级/用户级 token）

用法:
  # 路线 A（专享版）
  python3 enterprise_batch_register.py route-a \
      --base https://{enterpriseId}.copilot.qq.com \
      --enterprise-id 1234567890 \
      --api-key pt_xxxxxxxx \
      --emails ./emails.txt

  # 路线 B（SaaS 走 OneID）
  python3 enterprise_batch_register.py route-b \
      --oneid-base https://api.identity.tencent.com \
      --token "应用级 access_token" \
      --users ./users.jsonl

emails.txt 格式（每行一个，可带用户名/密码，逗号分隔）:
  alice@example.com
  bob@example.com,Bob,Passw0rd123

users.jsonl 格式（OneID，每行一个 JSON）:
  {"name":"张三","phone":"+8613800138000","email":"zhang@example.com"}
"""
import argparse
import json
import sys
import time
import urllib.request
import urllib.error

UA = {"User-Agent": "CodeBuddy-Enterprise-Batch/1.0"}


def http_json(method, url, token=None, body=None, timeout=30):
    headers = dict(UA)
    headers["Accept"] = "application/json"
    if token:
        headers["Authorization"] = "Bearer " + token
    data = None
    if body is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode()
            try:
                return r.status, json.loads(raw)
            except json.JSONDecodeError:
                return r.status, {"_raw": raw}
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except json.JSONDecodeError:
            return e.code, {"_raw": raw}
    except urllib.error.URLError as e:
        return 0, {"_error": str(e)}


# ───────────────────────── 路线 A ─────────────────────────

def route_a(args):
    base = args.base.rstrip("/")
    ep = f"{base}/api/v1/enterprises/{args.enterprise_id}/openapi/members/add"

    members = []
    with open(args.emails, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            parts = [p.strip() for p in line.split(",")]
            email = parts[0]
            username = parts[1] if len(parts) > 1 and parts[1] else email.split("@")[0]
            pwd = parts[2] if len(parts) > 2 and parts[2] else "Tencent@2026"
            members.append({"username": username, "email": email, "initialPassword": pwd})

    print(f"[route-a] 目标: {ep}")
    print(f"[route-a] 待添加成员: {len(members)} 个")

    results = []
    for i in range(0, len(members), args.batch):
        chunk = members[i:i + args.batch]
        body = {"members": chunk, "grantLicense": not args.no_license}
        code, resp = http_json("POST", ep, token=args.api_key, body=body)
        print(f"  批次 {i // args.batch + 1}: HTTP {code} → {json.dumps(resp, ensure_ascii=False)[:300]}")
        if code != 200 or resp.get("code") != 0:
            print("  !! 失败，跳过后续批次")
            results.append({"chunk": chunk, "resp": resp})
            continue
        data = resp.get("data") or {}
        for s in data.get("successList", []):
            results.append({"username": s.get("username"), "userId": s.get("userId"),
                            "initialPassword": s.get("initialPassword"),
                            "licenseGranted": s.get("licenseGranted")})
        for fl in data.get("failedList", []):
            results.append({"username": fl.get("username"), "error": fl.get("code"), "reason": fl.get("reason")})
        time.sleep(args.delay)

    out = args.out
    with open(out, "w", encoding="utf-8") as f:
        json.dump(results, f, ensure_ascii=False, indent=2)
    ok = sum(1 for r in results if "userId" in r)
    print(f"\n[route-a] 完成: {ok}/{len(members)} 成功，结果落盘 {out}")


# ───────────────────────── 路线 B ─────────────────────────

def route_b(args):
    ep = args.oneid_base.rstrip("/") + "/openapi/v3/contacts/users"
    users = []
    with open(args.users, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            users.append(json.loads(line))

    print(f"[route-b] 目标: {ep}")
    print(f"[route-b] 待创建用户: {len(users)} 个")

    results = []
    for u in users:
        code, resp = http_json("POST", ep, token=args.token, body=u)
        print(f"  {u.get('name')}/{u.get('phone')}: HTTP {code} → {json.dumps(resp, ensure_ascii=False)[:200]}")
        if code == 200 and resp.get("code") == 0:
            results.append({"input": u, "ok": True, "data": resp.get("data")})
        else:
            results.append({"input": u, "ok": False, "resp": resp})
        time.sleep(args.delay)

    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(results, f, ensure_ascii=False, indent=2)
    ok = sum(1 for r in results if r.get("ok"))
    print(f"\n[route-b] 完成: {ok}/{len(users)} 成功，结果落盘 {args.out}")


# ───────────────────────── 主入口 ─────────────────────────

def main():
    p = argparse.ArgumentParser(description="CodeBuddy 企业版批量产号")
    sub = p.add_subparsers(dest="cmd", required=True)

    a = sub.add_parser("route-a", help="专享版/私有化 members/add（绕开手机号）")
    a.add_argument("--base", required=True, help="Base URL，如 https://{enterpriseId}.copilot.qq.com")
    a.add_argument("--enterprise-id", required=True)
    a.add_argument("--api-key", required=True, help="pt_ 开头的企业 API Key")
    a.add_argument("--emails", required=True, help="邮箱列表文件")
    a.add_argument("--batch", type=int, default=100, help="每批数量，≤100")
    a.add_argument("--no-license", action="store_true", help="不发 License（默认发）")
    a.add_argument("--delay", type=float, default=1.0)
    a.add_argument("--out", default="enterprise_batch_result.json")
    a.set_defaults(fn=route_a)

    b = sub.add_parser("route-b", help="SaaS 旗舰版走 OneID 通讯录")
    b.add_argument("--oneid-base", default="https://api.identity.tencent.com")
    b.add_argument("--token", required=True, help="OneID 应用级 access_token")
    b.add_argument("--users", required=True, help="users.jsonl 文件")
    b.add_argument("--delay", type=float, default=1.0)
    b.add_argument("--out", default="oneid_batch_result.json")
    b.set_defaults(fn=route_b)

    args = p.parse_args()
    args.fn(args)


if __name__ == "__main__":
    main()
