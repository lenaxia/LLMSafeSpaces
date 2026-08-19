#!/usr/bin/env python3
"""Static file server (no-store) + WebSocket echo endpoint at /ws, same port.

WS handshake + frame echo implemented raw (no dependencies). All WS events
are logged to stderr -> server.log, which lets us tell proxy-level failures
(no log entry) from mid-connection failures (log entry + error).
"""
import base64
import hashlib
import http.server
import socketserver
import struct
import sys
from pathlib import Path

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
        if (self.path.split("?")[0].rstrip("/") == "/ws"
                and self.headers.get("Upgrade", "").lower() == "websocket"):
            self.handle_websocket()
            return
        super().do_GET()

    # ---------- websocket ----------
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
                if opcode == 8:                      # close
                    self.send_frame(8, payload[:2])
                    self.log_message("WS CLOSE ok")
                    break
                if opcode == 9:                      # ping -> pong
                    self.send_frame(10, payload)
                    continue
                if opcode in (1, 2):                 # text/binary -> echo
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
        print(f"serving {ROOT} on :{PORT} (no-store + ws echo at /ws)", flush=True)
        httpd.serve_forever()
