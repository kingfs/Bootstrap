#!/usr/bin/env python3
"""在真 PTY 里驱动 bs 的交互界面 —— 交互式 UI 的可重复验证工具。

为什么需要这个脚本：`internal/ui` 的花哨界面（charmbracelet/huh）只有拿到真终端才会启动，
而单元测试和 `scripts/smoke.sh` 里没有终端。更要紧的是**假终端会让按键"失灵"**：

    termenv/bubbletea 启动时会往终端写 OSC 11（查背景色）和 DSR（查光标位置），
    然后从 stdin 读回应。真终端立刻回答；`script` 这类假终端没人回答，于是把用户按键
    当成查询回应吞掉，看起来就像"按键没反应"。

所以这里扮演一个会回答查询的终端（并且先把窗口尺寸设成 --cols × --rows，
否则 huh 在 0x0 的终端里会把整段表单渲染成空），再把按键按时间表送进去，
最后打印去转义后的输出。

用法（全部在 .cache/pty 沙箱里读写，不发网络请求）：

    # 交互式向导：↓ 选第二项 → 回车 → 干跑
    python3 scripts/pty-check.py --keys DOWN ENTER -- init --dry-run

    # 校验行为：Ctrl-C 取消必须退出码 0 且提示"未做任何修改"
    python3 scripts/pty-check.py --expect-code 0 --expect-text 已取消 \
        --keys DOWN DOWN ENTER WAIT2 CTRLC -- init --dry-run

按键名：DOWN / UP / ENTER / SPACE / TAB / ESC / CTRLC / WAIT<n>（等 n 秒）/ 其它按字面量发。
"""

import argparse
import fcntl
import os
import pty
import re
import select
import struct
import sys
import termios
import time

KEYMAP = {
    "DOWN": b"\x1b[B",
    "UP": b"\x1b[A",
    "ENTER": b"\r",
    "SPACE": b" ",
    "TAB": b"\t",
    "ESC": b"\x1b",
    "CTRLC": b"\x03",
}

STRIP_OSC = re.compile(r"\x1b\][^\x07\x1b]*(\x07|\x1b\\)")
STRIP_CSI = re.compile(r"\x1b\[[0-9;?]*[a-zA-Z]")


def strip_ansi(raw: bytes) -> str:
    text = STRIP_OSC.sub("", raw.decode("utf-8", "replace"))
    text = STRIP_CSI.sub("", text)
    return text.replace("\r\n", "\n").replace("\r", "\n")


def build_schedule(keys, start, gap):
    """把按键列表变成 (绝对时间, 字节) 排期；WAIT<n> 表示额外等待。"""
    schedule = []
    at = start
    for k in keys:
        if k.startswith("WAIT"):
            at += float(k[4:])
            continue
        schedule.append((at, KEYMAP.get(k, k.encode())))
        at += gap
    return schedule


def main() -> int:
    ap = argparse.ArgumentParser(description="在 PTY 里驱动 bs 交互界面")
    ap.add_argument("--bin", default="bin/bs", help="要跑的 bs 二进制（默认 bin/bs）")
    ap.add_argument("--root", default=".cache/pty", help="沙箱目录（默认 .cache/pty）")
    ap.add_argument("--timeout", type=float, default=25.0, help="总超时秒数")
    ap.add_argument("--first-key-delay", type=float, default=1.5, help="首个按键前的等待")
    ap.add_argument("--key-gap", type=float, default=0.7, help="按键间隔秒数")
    ap.add_argument("--keys", nargs="*", default=[], help="按键序列，见文件头说明")
    ap.add_argument("--cols", type=int, default=100, help="伪终端宽度（默认 100）")
    ap.add_argument("--rows", type=int, default=30, help="伪终端高度（默认 30）")
    ap.add_argument(
        "--path",
        default=None,
        help="覆盖子进程 PATH（用 /nonexistent 可让所有工具探测为缺失，便于可重复地触发确认框）",
    )
    ap.add_argument("--expect-code", type=int, default=None, help="断言退出码")
    ap.add_argument("--expect-text", action="append", default=[], help="断言输出包含该文本")
    ap.add_argument("--dump", default=None, help="把去转义后的完整输出写到该文件")
    ap.add_argument("--quiet", action="store_true", help="只在失败时打印输出")
    ap.add_argument("cmd", nargs=argparse.REMAINDER, help="-- 之后是 bs 的参数")
    args = ap.parse_args()

    cmd = args.cmd[1:] if args.cmd[:1] == ["--"] else args.cmd
    if not cmd:
        cmd = ["init", "--dry-run"]

    root = os.path.abspath(args.root)
    for sub in ("home", "config", "state", "cache"):
        os.makedirs(os.path.join(root, sub), exist_ok=True)

    env = dict(os.environ)
    env.update(
        HOME=os.path.join(root, "home"),
        BS_CONFIG_HOME=os.path.join(root, "config"),
        BS_STATE_HOME=os.path.join(root, "state"),
        BS_CACHE_HOME=os.path.join(root, "cache"),
        TERM="xterm-256color",
    )
    env.pop("CI", None)
    env.pop("NO_COLOR", None)
    if args.path is not None:
        env["PATH"] = args.path

    binary = os.path.abspath(args.bin)
    if not os.access(binary, os.X_OK):
        print(f"找不到可执行的 {binary}（先 make build）", file=sys.stderr)
        return 2

    start = time.time()
    schedule = build_schedule(args.keys, start + args.first_key_delay, args.key_gap)

    # 自己开 PTY 而不是 pty.fork()：fork 之前先把窗口尺寸写好，子进程一启动就查得到。
    # 尺寸是 0x0 时 huh 会把整段表单渲染成空（只留下帮助行），断言"看得见哪些选项"就无从谈起。
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", args.rows, args.cols, 0, 0))
    pid = os.fork()
    if pid == 0:
        os.close(master)
        os.setsid()
        fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
        for target in (0, 1, 2):
            os.dup2(slave, target)
        if slave > 2:
            os.close(slave)
        os.execve(binary, [binary] + cmd, env)
    os.close(slave)
    fd = master

    chunks: list[bytes] = []
    code = None
    timed_out = False
    deadline = start + args.timeout
    while time.time() < deadline and code is None:
        r, _, _ = select.select([fd], [], [], 0.2)
        if r:
            try:
                data = os.read(fd, 65536)
            except OSError:
                break
            if not data:
                break
            chunks.append(data)
            # 扮演终端：回答色彩与光标查询，否则 bubbletea 会把按键当回应吞掉。
            if b"\x1b]11;?" in data:
                os.write(fd, b"\x1b]11;rgb:0000/0000/0000\x1b\\")
            if b"\x1b[6n" in data:
                os.write(fd, b"\x1b[1;1R")
        while schedule and time.time() >= schedule[0][0]:
            os.write(fd, schedule.pop(0)[1])
        done, status = os.waitpid(pid, os.WNOHANG)
        if done:
            code = os.waitstatus_to_exitcode(status)
            break

    # 读到 EIO/EOF 只是说明子进程关了 PTY，未必已经回收；先给它一小会儿自己退出，
    # 否则会把"正常退出"误报成超时（曾经的 bug：exit code 是 0 却提示超时）。
    for _ in range(20):
        if code is not None:
            break
        done, status = os.waitpid(pid, os.WNOHANG)
        if done:
            code = os.waitstatus_to_exitcode(status)
            break
        time.sleep(0.05)

    if code is None:
        timed_out = True
        os.kill(pid, 9)
        _, status = os.waitpid(pid, 0)
        code = os.waitstatus_to_exitcode(status)

    text = strip_ansi(b"".join(chunks))
    if args.dump:
        with open(args.dump, "w", encoding="utf-8") as fh:
            fh.write(text)
    lines = [ln for ln in text.split("\n") if ln.strip()]

    problems = []
    if timed_out:
        problems.append(f"超时 {args.timeout:.0f}s 后仍未退出（退出码按 -9 记）")
    if args.expect_code is not None and code != args.expect_code:
        problems.append(f"退出码 {code}，期望 {args.expect_code}")
    for want in args.expect_text:
        if want not in text:
            problems.append(f"输出里找不到 {want!r}")

    if problems or not args.quiet:
        print(f"== bs {' '.join(cmd)}")
        print(f"== 按键: {' '.join(args.keys) or '(无)'}   退出码: {code}")
        print("== 输出（去转义，最后 40 行）:")
        print("\n".join(lines[-40:]))
    for p in problems:
        print(f"FAIL: {p}", file=sys.stderr)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
