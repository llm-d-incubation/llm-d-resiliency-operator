# Copyright 2026 The llm-d Authors.
# SPDX-License-Identifier: Apache-2.0
"""Keep Pod-scoped process-exit evidence after a rank's API endpoint disappears."""

import argparse
import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class ProcessObserver:
    def __init__(self, pod_uid, ranks):
        self.pod_uid = pod_uid
        self.names = {
            rank: {
                "api": f"VLLM::APIServer_DP{rank}",
                "engine": f"VLLM::EngineCore_DP{rank}",
                "worker": f"VLLM::Worker_DP{rank}_EP{rank}",
            }
            for rank in ranks
        }
        self.identities = {}
        self.lock = threading.Lock()
        self.current = {"schema_version": 1, "pod_uid": pod_uid, "ranks": []}

    def sample(self, processes):
        unknown = {
            row["pid"] for row in processes if any(row[key] is None for key in ["name", "create_time", "status"])
        }
        live = {
            row["name"]: (row["pid"], row["create_time"])
            for row in processes
            if row["pid"] not in unknown and row["status"] not in {"zombie", "dead"}
        }
        ranks = []
        for rank, roles in self.names.items():
            observed = {}
            for role, name in roles.items():
                identity = live.get(name)
                key = (rank, role)
                if identity is not None and key not in self.identities:
                    self.identities[key] = identity
                original = self.identities.get(key)
                observed[role] = {
                    "observed": original is not None,
                    "alive": original is not None and identity == original,
                    "unknown": original is not None and original[0] in unknown,
                }
            lost = [
                role
                for role in ["engine", "worker"]
                if observed[role]["observed"] and not observed[role]["alive"] and not observed[role]["unknown"]
            ]
            ranks.append(
                {
                    "id": rank,
                    "engine_dead": bool(lost),
                    "reason": "process exited or replaced: " + ", ".join(lost) if lost else "",
                    "processes": observed,
                }
            )
        with self.lock:
            self.current = {"schema_version": 1, "pod_uid": self.pod_uid, "ranks": ranks}

    def snapshot(self):
        with self.lock:
            return self.current


def main():
    import psutil

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--start-rank", type=int, required=True)
    parser.add_argument("--local-ranks", type=int, required=True)
    parser.add_argument("--host", default="0.0.0.0")  # noqa: S104 -- Pod diagnostic endpoint
    parser.add_argument("--port", type=int, default=9257)
    args = parser.parse_args()
    if args.start_rank < 0 or args.local_ranks < 1:
        parser.error("start-rank must be non-negative and local-ranks positive")
    observer = ProcessObserver(os.environ["POD_UID"], range(args.start_rank, args.start_rank + args.local_ranks))
    stop = threading.Event()

    def sample():
        while not stop.is_set():
            rows = []
            for process in psutil.process_iter(["pid", "name", "create_time", "status"]):
                try:
                    rows.append(process.info)
                except psutil.NoSuchProcess:
                    continue
                except psutil.AccessDenied:
                    rows.append({"pid": process.pid, "name": None, "create_time": None, "status": None})
            observer.sample(rows)
            stop.wait(0.25)

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802 -- standard library handler contract
            if self.path != "/status":
                self.send_error(404)
                return
            body = json.dumps(observer.snapshot()).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_args):
            pass

    server = ThreadingHTTPServer((args.host, args.port), Handler)
    thread = threading.Thread(target=sample, daemon=True)
    thread.start()
    try:
        server.serve_forever()
    finally:
        stop.set()
        server.server_close()
        thread.join(timeout=2)


if __name__ == "__main__":
    main()
