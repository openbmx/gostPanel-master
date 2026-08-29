#!/usr/bin/env python3
"""校验 npm audit 结果：所有 high/critical 漏洞都必须有一条已登记且未过期的例外。

设计意图：
  常见做法是给 `npm audit` 加上 `|| true`，结果是扫描永远绿灯、等于没扫。
  这里改为「默认失败，例外需显式登记」——每条例外必须写明缓解措施与到期日，
  到期后 CI 会重新变红，逼迫复查而不是无限期挂着。

例外清单格式（.github/audit-exceptions.yml）：

    version: 1
    exceptions:
      - package: "some-lib"
        advisory: "GHSA-xxxx-xxxx-xxxx"   # GHSA / CVE / advisory URL 均可
        severity: "high"
        mitigation: "仅构建期使用，不进入运行时产物"
        expires_on: "2026-12-31"
"""

import argparse
import json
import sys
from datetime import date

HIGH_SEVERITIES = {"high", "critical"}
REQUIRED_FIELDS = ("package", "advisory", "severity", "mitigation", "expires_on")


def split_kv(line):
    """解析 "key: value" 形式的简单 YAML 行并去除引号。

    刻意不引入 PyYAML：CI 里少一个依赖就少一处供应链风险，
    而这份清单的结构简单到不值得为它装一个解析器。
    """
    key, value = line.split(":", 1)
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
        value = value[1:-1]
    return key.strip(), value


def parse_exceptions(path):
    exceptions = []
    current = None
    with open(path, "r", encoding="utf-8") as handle:
        for raw in handle:
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith(("version:", "exceptions:")):
                continue
            if line.startswith("- "):
                if current:
                    exceptions.append(current)
                current = {}
                rest = line[2:].strip()
                if rest and ":" in rest:
                    key, value = split_kv(rest)
                    current[key] = value
                continue
            if current is not None and ":" in line:
                key, value = split_kv(line)
                current[key] = value
    if current:
        exceptions.append(current)
    return exceptions


def iter_vulns(data):
    """遍历 npm audit --json 的 vulnerabilities 结构。

    npm 会把同一个包的多条 advisory 放进 via 数组；via 里也可能是字符串
    （表示经由另一个包间接引入）。两种形态都要覆盖，否则会漏报。
    """
    vulnerabilities = data.get("vulnerabilities")
    if not isinstance(vulnerabilities, dict):
        return

    for name, vuln in vulnerabilities.items():
        severity = vuln.get("severity")
        via = vuln.get("via", [])
        if isinstance(via, (str, dict)):
            via = [via]
        if not isinstance(via, list):
            continue

        for item in via:
            if isinstance(item, dict):
                advisory = (
                    item.get("url")
                    or item.get("source")
                    or item.get("title")
                    or item.get("name")
                )
                title = item.get("title") or item.get("url") or ""
            else:
                advisory = str(item)
                title = str(item)
            if advisory:
                yield name, severity, str(advisory), title


def norm(value):
    return str(value).strip().lower() if value is not None else ""


def parse_date(value):
    try:
        return date.fromisoformat(value)
    except (ValueError, TypeError):
        return None


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--audit", required=True)
    parser.add_argument("--exceptions", required=True)
    args = parser.parse_args()

    try:
        with open(args.audit, "r", encoding="utf-8") as handle:
            audit = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        # 审计文件缺失/损坏必须视为失败，否则扫描会静默跳过
        sys.stderr.write("无法读取 npm audit 结果 %s: %s\n" % (args.audit, exc))
        return 1

    errors = []
    index = {}

    for exc in parse_exceptions(args.exceptions):
        missing = [f for f in REQUIRED_FIELDS if not exc.get(f)]
        if missing:
            errors.append(
                "例外缺少必填字段 %s: %s" % (missing, exc.get("package", "<unknown>"))
            )
            continue
        expires = parse_date(exc.get("expires_on"))
        if expires is None:
            errors.append("例外的 expires_on 非法（需 YYYY-MM-DD）: %s" % exc.get("package"))
            continue
        key = (norm(exc["package"]), norm(exc["advisory"]))
        if key in index:
            errors.append("重复的例外: %s / %s" % (exc["package"], exc["advisory"]))
            continue
        index[key] = {"severity": norm(exc["severity"]), "expires_on": expires}

    today = date.today()
    missing_exceptions = []
    expired = []
    seen = set()

    for name, severity, advisory, title in iter_vulns(audit):
        sev = norm(severity)
        if sev not in HIGH_SEVERITIES or not name:
            continue
        key = (norm(name), norm(advisory))
        if key in seen:
            continue
        seen.add(key)

        exc = index.get(key)
        if exc is None:
            missing_exceptions.append((name, sev, advisory, title))
            continue
        if exc["severity"] and exc["severity"] != sev:
            errors.append(
                "例外的 severity 与实际不符: %s [%s] 实际 %s，登记 %s"
                % (name, advisory, sev, exc["severity"])
            )
        if exc["expires_on"] < today:
            expired.append((name, sev, advisory, exc["expires_on"].isoformat()))

    if missing_exceptions:
        errors.append("发现未登记例外的 high/critical 漏洞：")
        for name, sev, advisory, title in missing_exceptions:
            label = "%s (%s) [%s]" % (name, sev, advisory)
            if title and title != advisory:
                label += ": %s" % title
            errors.append("  - " + label)
        errors.append(
            "请先升级依赖；确无法升级时，在 .github/audit-exceptions.yml 中登记例外"
            "（须写明缓解措施与到期日）。"
        )

    if expired:
        errors.append("以下例外已过期，需重新评估：")
        for name, sev, advisory, expires_on in expired:
            errors.append("  - %s (%s) [%s] 于 %s 过期" % (name, sev, advisory, expires_on))

    if errors:
        sys.stderr.write("\n".join(errors) + "\n")
        return 1

    print("npm audit 检查通过：无未登记的 high/critical 漏洞。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
