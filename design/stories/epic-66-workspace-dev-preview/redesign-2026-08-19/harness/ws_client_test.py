#!/usr/bin/env python3
"""Raw WebSocket client for local verification of the /ws echo endpoint.

Usage: python3 ws_client_test.py [port]
Exits 0 with LOCAL WS TEST: PASS if handshake + echo round-trip succeed.
Run INSIDE the pod against the local server — this tests the server, NOT the tunnel.
"""
import base64
import os
import socket
import sys

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 5173

key = base64.b64encode(os.urandom(16)).decode()
s = socket.create_connection(("127.0.0.1", PORT), timeout=5)
s.sendall((
    f"GET /ws HTTP/1.1\r\nHost: 127.0.0.1:{PORT}\r\nUpgrade: websocket\r\n"
    f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\n"
    f"Sec-WebSocket-Version: 13\r\n\r\n"
).encode())

resp = b""
while b"\r\n\r\n" not in resp:
    resp += s.recv(4096)
status = resp.split(b"\r\n")[0].decode()
print("handshake:", status)
assert "101" in status, "handshake failed"

payload = b"hello-from-tunnel"
mask = os.urandom(4)
masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
s.sendall(bytes([0x81, 0x80 | len(payload)]) + mask + masked)

hdr = b""
while len(hdr) < 2:
    hdr += s.recv(2 - len(hdr))
ln = hdr[1] & 0x7F
data = b""
while len(data) < ln:
    data += s.recv(ln - len(data))
print("echo:", data.decode())
assert data == payload
print("LOCAL WS TEST: PASS")
s.close()
