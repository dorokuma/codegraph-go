#!/usr/bin/env python3
# 用途: 清理 codegraph daemon 注册表目录中匹配指定 PID 或 workdir 的记录文件，保留畸形与其它项目记录
# 运行方式: python3 scripts/cleanup_daemon_registry.py <registry_dir> [killed_pid] [workdir]
# 依赖环境: python3 (标准库 json, os, sys)

import json
import os
import sys


def cleanup_registry(reg_dir: str, killed_pid_str: str = "", workdir: str = "") -> None:
    killed_pid = None
    if killed_pid_str:
        try:
            killed_pid = int(killed_pid_str)
        except ValueError:
            pass

    if not os.path.isdir(reg_dir):
        return

    try:
        entries = os.listdir(reg_dir)
    except OSError as err:
        print(f"DEPLOY FAILED: cannot list registry dir {reg_dir}: {err}", file=sys.stderr)
        sys.exit(1)

    for fname in entries:
        if not fname.endswith(".json"):
            continue
        fpath = os.path.join(reg_dir, fname)

        try:
            with open(fpath, "r", encoding="utf-8") as fp:
                try:
                    data = json.load(fp)
                except (json.JSONDecodeError, UnicodeDecodeError, ValueError):
                    # malformed JSON: preserve and do not delete
                    continue
        except OSError as err:
            print(f"DEPLOY FAILED: error reading registry record {fpath}: {err}", file=sys.stderr)
            sys.exit(1)

        if not isinstance(data, dict):
            # non-object JSON: preserve and do not delete
            continue

        rec_pid = data.get("pid")
        rec_root = data.get("root")

        match = False
        if killed_pid is not None and rec_pid is not None:
            try:
                if int(rec_pid) == killed_pid:
                    match = True
            except (ValueError, TypeError):
                pass
        if not match and workdir and rec_root is not None:
            if str(rec_root) == workdir:
                match = True

        if match:
            try:
                os.remove(fpath)
            except FileNotFoundError:
                pass
            except OSError as err:
                print(f"DEPLOY FAILED: error removing registry record {fpath}: {err}", file=sys.stderr)
                sys.exit(1)


def main() -> None:
    if len(sys.argv) < 2:
        print(f"Usage: {sys.argv[0]} <registry_dir> [killed_pid] [workdir]", file=sys.stderr)
        sys.exit(1)

    reg_dir = sys.argv[1]
    killed_pid_str = sys.argv[2] if len(sys.argv) > 2 else ""
    workdir = sys.argv[3] if len(sys.argv) > 3 else ""

    cleanup_registry(reg_dir, killed_pid_str, workdir)


if __name__ == "__main__":
    main()
