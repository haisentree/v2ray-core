#!/usr/bin/env python3
"""v2ray 链路端到端验证:socks 入站 → dispatcher → freedom 出站 → 本地 HTTP 目标。

对应 rain-net 项目 docs/research/v2ray-core.md §八 链路模型的冒烟测试。
SOCKS5 握手为手写实现,仅用标准库,不依赖 PySocks / curl。

用法(二选一):
  # 1. 已有 v2ray 跑在 127.0.0.1:10808(配置含 socks 入站 + freedom 出站),直接测:
  ./test_socks_chain.py

  # 2. 让脚本自己拉起 v2ray 与目标 HTTP 服务,测完自动清理:
  ./test_socks_chain.py --v2ray /tmp/v2ray-core/v2ray --config /tmp/v2ray-core/config.json

可选参数:--socks 127.0.0.1:10808  --host 127.0.0.1  --port 18080  --repeat 3


# 脚本自己拉起 v2ray + 目标 HTTP 服务,测完自动清理(推荐):
python3 test_socks_chain.py --v2ray /tmp/v2ray-core/v2ray --config /tmp/v2ray-core/config.json

# v2ray 已在跑时,直接测:
python3 test_socks_chain.py            # 默认 socks=127.0.0.1:10808,目标 18080,3 次

"""




import argparse
import http.server
import socket
import subprocess
import sys
import threading
import time

BODY = b"v2ray link-chain test OK\n"


def socks5_connect(socks_addr, dst_ip, dst_port, timeout=5):
    """手写 SOCKS5 CONNECT:no-auth 握手 → 连接 dst → 返回已就绪的 socket。"""
    s = socket.create_connection(socks_addr, timeout=timeout)
    s.sendall(b"\x05\x01\x00")  # greeting: ver=5, 1 个方法, no-auth(0x00)
    r = s.recv(2)
    if r != b"\x05\x00":
        raise AssertionError(f"SOCKS5 greeting 失败: {r!r}")
    # request: ver=5 cmd=CONNECT(1) rsv=0 atyp=IPv4(1) addr port
    s.sendall(b"\x05\x01\x00\x01"
              + bytes(int(x) for x in dst_ip.split("."))
              + dst_port.to_bytes(2, "big"))
    r = s.recv(4)
    if len(r) != 4 or r[1] != 0:
        raise AssertionError(f"SOCKS5 CONNECT 失败(rep={r[1:2]!r})")
    # 跳过 BND.ADDR/PORT(atyp: 1=IPv4, 4=IPv6, 3=domain)
    skip = {1: 4, 4: 16}.get(r[3])
    if skip is not None:
        s.recv(skip)
    else:
        s.recv(1)  # domain 长度前缀
        s.recv(255)
    s.recv(2)
    return s


def http_get_over_socks(socks_addr, host, port):
    """经 socks 代理向 host:port 发 HTTP/1.1 GET,返回完整响应字节。"""
    s = socks5_connect(socks_addr, host, port)
    try:
        s.sendall(f"GET / HTTP/1.1\r\nHost: {host}\r\nConnection: close\r\n\r\n".encode())
        chunks = []
        while True:
            b = s.recv(4096)
            if not b:
                break
            chunks.append(b)
        return b"".join(chunks)
    finally:
        s.close()


class Target:
    """内置目标 HTTP 服务:固定响应 BODY,供 freedom 出站回源。"""

    def __init__(self, host, port):
        self.httpd = http.server.ThreadingHTTPServer((host, port), _Handler)
        self.thread = None

    def start(self):
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()

    def stop(self):
        self.httpd.shutdown()
        self.httpd.server_close()


class _Handler(http.server.BaseHTTPRequestHandler):
    body = BODY

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(self.body)))
        self.end_headers()
        self.wfile.write(self.body)

    def log_message(self, *a):  # 静音访问日志
        pass


def wait_listening(addr, deadline=5.0):
    """轮询直到 addr 可连(v2ray 起动等待)。"""
    end = time.time() + deadline
    while time.time() < end:
        try:
            socket.create_connection(addr, timeout=0.5).close()
            return
        except OSError:
            time.sleep(0.1)
    raise TimeoutError(f"{addr} 未在 {deadline}s 内开始监听")


def main():
    ap = argparse.ArgumentParser(description="v2ray socks→freedom 链路冒烟测试")
    ap.add_argument("--socks", default="127.0.0.1:10808")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=18080)
    ap.add_argument("--v2ray", help="v2ray 可执行路径;给出则由脚本拉起并负责清理")
    ap.add_argument("--config", help="v2ray 配置文件;--v2ray 时必填")
    ap.add_argument("--repeat", type=int, default=3)
    args = ap.parse_args()
    socks_addr = tuple(args.socks.rsplit(":", 1))
    socks_addr = (socks_addr[0], int(socks_addr[1]))

    procs, target = [], None
    try:
        target = Target(args.host, args.port)
        target.start()
        if args.v2ray:
            if not args.config:
                ap.error("--v2ray 需要同时给 --config")
            log = open("/tmp/v2ray-test.log", "ab")
            procs.append(subprocess.Popen(
                [args.v2ray, "run", "-c", args.config], stdout=log, stderr=log))
            wait_listening(socks_addr)
        for i in range(args.repeat):
            resp = http_get_over_socks(socks_addr, args.host, args.port)
            line = resp.split(b"\r\n", 1)[0]
            if b" 200 " not in line or not resp.endswith(BODY):
                raise AssertionError(f"第 {i + 1} 次响应异常: {line!r}")
            print(f"[{i + 1}/{args.repeat}] PASS {line.decode()}")
        print(f"ALL PASS: {args.repeat} 次经 socks({args.socks}) → freedom → "
              f"{args.host}:{args.port} 全部成功")
    finally:
        for p in procs:
            p.terminate()
        if target:
            target.stop()


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print(f"FAIL: {e}", file=sys.stderr)
        sys.exit(1)
