#!/usr/bin/env python3
"""Best-effort v2 state report. Python standard library only; stdout stays empty."""
import argparse
import http.client
import ipaddress
import json
from pathlib import Path
import socket
import subprocess
import sys
import threading
import time
import urllib.parse
import uuid


def connection(directory):
    with (Path(directory) / "private" / "ingest.json").open("rb") as source:
        raw = source.read(16385)
    if len(raw) > 16384:
        raise ValueError("invalid_connection")
    config = json.loads(raw)
    address = urllib.parse.urlsplit(config["url"])
    token = config["token"]
    if (address.scheme != "http" or address.username is not None
            or address.password is not None or address.path not in ("", "/")
            or address.query or address.fragment
            or not ipaddress.ip_address(address.hostname).is_loopback
            or not isinstance(token, str) or not token or len(token) > 4096
            or any(ord(c) < 32 or 127 <= ord(c) <= 159 for c in token)):
        raise ValueError("invalid_connection")
    return address.hostname, address.port or 80, token


def send_http(directory, payload):
    deadline = time.monotonic() + 1
    for attempt in range(2):
        host, port, token = connection(directory)  # Re-read after token rotation.
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError()
        conn = http.client.HTTPConnection(host, port, timeout=remaining)
        timer = None
        try:
            conn.connect()  # Literal IP only; no DNS, proxy or redirects.
            wire_socket = conn.sock

            def expire():
                try:
                    wire_socket.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass

            timer = threading.Timer(max(0, deadline - time.monotonic()), expire)
            timer.daemon = True
            timer.start()
            conn.request("POST", "/v2/ingest", payload, {
                "Content-Type": "application/json",
                "Authorization": "Bearer " + token,
            })
            response = conn.getresponse()
            if response.status == 401 and attempt == 0:
                continue
            if response.status not in (200, 201):
                raise ValueError("report_http_error")
            body = response.read(4097)
            if len(body) > 4096:
                raise ValueError("invalid_ack")
            ack = json.loads(body)
            if (ack.get("schema") != 2 or type(ack.get("id")) is not int
                    or ack["id"] < 1 or not ack.get("epoch")):
                raise ValueError("invalid_ack")
            return
        except (OSError, http.client.HTTPException):
            if attempt or time.monotonic() >= deadline:
                raise
        finally:
            if timer is not None:
                timer.cancel()
            conn.close()
    raise TimeoutError()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data-dir", required=True)
    parser.add_argument("--agent", required=True)
    parser.add_argument("--session", required=True)
    parser.add_argument("--operation", required=True)
    parser.add_argument("--producer", default="python-example")
    parser.add_argument("--namespace")
    parser.add_argument("--event-id", default=None)
    parser.add_argument("--state", choices=("working", "tool_activity", "waiting_confirmation",
                        "idle", "completed", "interrupted", "failed", "unknown", "offline"), required=True)
    parser.add_argument("--mode", choices=("cli", "http"), default="cli")
    parser.add_argument("--exe", default="agent-statusd")
    args = parser.parse_args()
    event = dict(schema=2, producer_id=args.producer, event_id=args.event_id or str(uuid.uuid4()),
                 agent=args.agent, session_id=args.session, operation_id=args.operation,
                 kind="state.reported", reported_basis="self_report", data={"state": args.state})
    if args.namespace:
        event["session_namespace"] = args.namespace
    payload = json.dumps(event, ensure_ascii=False).encode("utf-8")
    if len(payload) > 16384:
        raise ValueError("body_too_large")
    if args.mode == "cli":
        result = subprocess.run([args.exe, "report", "--data-dir", args.data_dir, "--best-effort"],
                                input=payload, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                                timeout=1, check=False)
        if result.returncode or result.stderr:
            raise ValueError("report_unavailable")
    else:
        send_http(args.data_dir, payload)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError,
            http.client.HTTPException):
        # Do not print exception text, credentials, request data or Runtime ACK.
        print("report_unavailable", file=sys.stderr)
    # This example never blocks or fails the enclosing Agent on a reporting error.
