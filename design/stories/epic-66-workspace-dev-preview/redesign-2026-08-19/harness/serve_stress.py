#!/usr/bin/env python3
"""serve_ws.py + /sse (drip stream) + /big (byte generator) for stress testing.

Endpoints:
  /sse?d=<ms-delay>&n=<chunks>  text/event-stream, drips n chunks d ms apart,
                                includes server timestamps per chunk
  /big?bytes=<n>                n bytes of application/octet-stream (max 16MB)
  /ws                           WebSocket echo (unchanged from serve_ws.py)
  everything else               static files from this dir, no-store
"""
import http.server
import socketserver
import sys
import time
import base64
import hashlib
import struct
from pathlib import Path
from urllib.parse import parse_qs

PORT = 5173
ROOT = Path(__file__).parent
WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


class Handler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(ROOT), **kwargs)

    def end_headers(self):
        self.send_header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
        self.send_header("Pragma", "no-cache")
        self.send_header("Expires", "0")
        super().end_headers()

    def do_GET(self):
        if "?" in self.path:
            path, qs = self.path.split("?", 1)
        else:
            path, qs = self.path, ""
        if path.rstrip("/") == "/ws" and self.headers.get("Upgrade", "").lower() == "websocket":
            return self.handle_websocket()
        if path == "/sse":
            return self.handle_sse(parse_qs(qs))
        if path == "/big":
            return self.handle_big(parse_qs(qs))
        super().do_GET()

    # ---------- SSE ----------
    def handle_sse(self, q):
        delay = min(float(q.get("d", ["200"])[0]) / 1000.0, 2.0)
        n = min(int(q.get("n", ["10"])[0]), 100)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        # deliberately NO Content-Length: close-delimited stream.
        self.end_headers()
        try:
            self.log_message("SSE START n=%d d=%dms", n, int(delay * 1000))
            for i in range(n):
                self.wfile.write(
                    f'data: {{"i": {i}, "t": {time.time():.3f}}}\n\n'.encode())
                self.wfile.flush()
                if i < n - 1:
                    time.sleep(delay)
            self.wfile.write(b"event: done\ndata: {}\n\n")
            self.wfile.flush()
            self.log_message("SSE DONE  n=%d", n)
        except Exception as exc:
            self.log_message("SSE ERR   %s", exc)
        self.close_connection = True

    # ---------- big ----------
    def handle_big(self, q):
        try:
            total = min(int(q.get("bytes", ["1048576"])[0]), 16 * 1024 * 1024)
        except ValueError:
            self.send_error(400, "bad bytes param")
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(total))
        self.end_headers()
        chunk = b"x" * 65536
        sent = 0
        try:
            while sent < total:
                n = min(65536, total - sent)
                self.wfile.write(chunk[:n])
                sent += n
            self.log_message("BIG OK    %d bytes", total)
        except Exception as exc:
            self.log_message("BIG ERR   %s after %d/%d", exc, sent, total)

    # ---------- websocket (identical to serve_ws.py) ----------
    def handle_websocket(self):
        key = self.headers.get("Sec-WebSocket-Key")
        if not key:
            self.send_error(400, "missing Sec-WebSocket-Key")
            return
        accept = base64.b64encode(
            hashlib.sha1((key + WS_GUID).encode()).digest()).decode()
        self.connection.sendall((
            "HTTP/1.1 101 Switching Protocols\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            f"Sec-WebSocket-Accept: {accept}\r\n\r\n"
        ).encode())
        self.connection.settimeout(300)
        self.log_message("WS OPEN  %s", self.path)
        try:
            while True:
                opcode, payload = self.read_frame()
                if opcode == 8:
                    self.send_frame(8, payload[:2])
                    self.log_message("WS CLOSE ok")
                    break
                if opcode == 9:
                    self.send_frame(10, payload)
                    continue
                if opcode in (1, 2):
                    self.log_message("WS ECHO  %d bytes", len(payload))
                    self.send_frame(opcode, payload)
        except Exception as exc:
            self.log_message("WS ERR   %s", exc)
        finally:
            try:
                self.connection.close()
            except Exception:
                pass

    def read_exact(self, n):
        buf = b""
        while len(buf) < n:
            chunk = self.rfile.read(n - len(buf))
            if not chunk:
                raise ConnectionError("client disconnected")
            buf += chunk
        return buf

    def read_frame(self):
        hdr = self.read_exact(2)
        opcode = hdr[0] & 0x0F
        masked = hdr[1] & 0x80
        length = hdr[1] & 0x7F
        if length == 126:
            length = struct.unpack(">H", self.read_exact(2))[0]
        elif length == 127:
            length = struct.unpack(">Q", self.read_exact(8))[0]
        mask = self.read_exact(4) if masked else None
        payload = self.read_exact(length) if length else b""
        if mask:
            payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        return opcode, payload

    def send_frame(self, opcode, payload):
        header = bytes([0x80 | opcode])
        n = len(payload)
        if n < 126:
            header += bytes([n])
        elif n < 65536:
            header += bytes([126]) + struct.pack(">H", n)
        else:
            header += bytes([127]) + struct.pack(">Q", n)
        self.connection.sendall(header + payload)

    def log_message(self, fmt, *args):
        sys.stderr.write("%s %s\n" % (self.address_string(), fmt % args))
        sys.stderr.flush()


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


if __name__ == "__main__":
    with Server(("", PORT), Handler) as httpd:
        print(f"serving {ROOT} on :{PORT} (no-store + ws + sse + big)", flush=True)
        httpd.serve_forever()
