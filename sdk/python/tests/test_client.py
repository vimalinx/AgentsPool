from __future__ import annotations

import json
import os
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit

from agentspool_sdk import APIError, AgentsPoolClient, TaskTerminalError


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    task_polls = 0

    def log_message(self, *_: object) -> None:
        pass

    def _send(self, status: int, payload: object, content_type: str = "application/json") -> None:
        data = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self) -> None:
        split = urlsplit(self.path)
        if self.headers.get("Authorization") != "Bearer admin":
            self._send(401, {"error": {"code": "unauthorized", "message": "bad token"}})
            return
        if split.path == "/api/v1/status":
            self._send(200, {"version": "test", "node_counts": {}})
        elif split.path == "/api/v1/tasks/task_1":
            Handler.task_polls += 1
            succeeded = Handler.task_polls >= 2
            self._send(
                200,
                {
                    "task": {
                        "id": "task_1",
                        "status": "succeeded" if succeeded else "running",
                        "result": {"output_artifact_ids": ["artifact_1"]} if succeeded else None,
                    },
                    "executions": [{"id": "exec_1", "attempt": 1}],
                    "active_lease": None,
                },
            )
        elif split.path == "/api/v1/tasks/task_bad":
            self._send(
                200,
                {
                    "task": {"id": "task_bad", "status": "needs_review", "last_error": "unsafe replay"},
                    "executions": [],
                    "active_lease": None,
                },
            )
        elif split.path == "/api/v1/tasks/task_1/logs":
            self._send(
                200,
                {
                    "execution_id": "exec_1",
                    "events": [{"sequence": 1, "stream": "stdout", "message": "hello"}],
                    "next_after": 1,
                },
            )
        elif split.path == "/api/v1/artifacts/artifact_1/content":
            self._send(200, b"artifact-data", "application/octet-stream")
        elif split.path == "/api/v1/nodes":
            self._send(200, {"nodes": [{"id": "node_1"}], "count": 1})
        else:
            self._send(404, {"error": {"code": "not_found", "message": split.path}})

    def do_POST(self) -> None:
        split = urlsplit(self.path)
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        if self.headers.get("Authorization") != "Bearer admin":
            self._send(401, {"error": {"code": "unauthorized", "message": "bad token"}})
            return
        if split.path == "/api/v1/tasks":
            spec = json.loads(body)
            self._send(201, {"task": {"id": "task_1", "status": "queued", "spec": spec}, "created": True})
        elif split.path == "/api/v1/artifacts/gc":
            request = json.loads(body)
            self._send(200, {"dry_run": not request.get("delete", False), "orphan_files": 1})
        elif split.path == "/api/v1/artifacts":
            self._send(
                201,
                {
                    "artifact": {
                        "id": "artifact_1",
                        "name": "input.bin",
                        "size": len(body),
                    }
                },
            )
        else:
            self._send(404, {"error": {"code": "not_found", "message": split.path}})

    def do_DELETE(self) -> None:
        split = urlsplit(self.path)
        if self.headers.get("Authorization") != "Bearer admin":
            self._send(401, {"error": {"code": "unauthorized", "message": "bad token"}})
            return
        if split.path == "/api/v1/tasks/task_1":
            self._send(200, {"deleted": True, "task_id": "task_1"})
        elif split.path == "/api/v1/artifacts/artifact_1":
            self._send(200, {"deleted": True, "artifact_id": "artifact_1", "forced": split.query == "force=true"})
        else:
            self._send(404, {"error": {"code": "not_found", "message": split.path}})


class ClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.url = f"http://127.0.0.1:{cls.server.server_port}"

    @classmethod
    def tearDownClass(cls) -> None:
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join(timeout=2)

    def setUp(self) -> None:
        Handler.task_polls = 0
        self.client = AgentsPoolClient(self.url, "admin", timeout=2)

    def test_status_and_nodes(self) -> None:
        self.assertEqual(self.client.status()["version"], "test")
        self.assertEqual(self.client.list_nodes()[0]["id"], "node_1")

    def test_submit_wait_and_logs(self) -> None:
        response = self.client.submit_task({"name": "x", "executor": {"kind": "shell", "script": "true"}})
        self.assertTrue(response["created"])
        events: list[dict] = []
        detail = self.client.wait_task("task_1", poll_interval=0.01, timeout=1, on_log=events.append)
        self.assertEqual(detail["task"]["status"], "succeeded")
        self.assertTrue(events)

    def test_terminal_error(self) -> None:
        with self.assertRaises(TaskTerminalError):
            self.client.wait_task("task_bad", poll_interval=0.01, timeout=1)

    def test_structured_api_error(self) -> None:
        bad = AgentsPoolClient(self.url, "wrong", timeout=2)
        with self.assertRaises(APIError) as context:
            bad.status()
        self.assertEqual(context.exception.status_code, 401)
        self.assertEqual(context.exception.code, "unauthorized")

    def test_atomic_download_and_overwrite_guard(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            destination = Path(directory) / "output.bin"
            self.client.download_artifact("artifact_1", destination)
            self.assertEqual(destination.read_bytes(), b"artifact-data")
            with self.assertRaises(FileExistsError):
                self.client.download_artifact("artifact_1", destination)
            self.client.download_artifact("artifact_1", destination, overwrite=True)
            self.assertFalse(any(path.suffix == ".part" for path in destination.parent.iterdir()))

    def test_upload(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "input.bin"
            path.write_bytes(b"12345")
            artifact = self.client.upload_artifact(path)
            self.assertEqual(artifact["size"], 5)

    def test_explicit_data_deletion(self) -> None:
        self.assertTrue(self.client.delete_task("task_1")["deleted"])
        artifact = self.client.delete_artifact("artifact_1", force=True)
        self.assertTrue(artifact["deleted"])
        self.assertTrue(artifact["forced"])
        self.assertTrue(self.client.gc_artifacts()["dry_run"])
        self.assertFalse(self.client.gc_artifacts(delete=True)["dry_run"])


if __name__ == "__main__":
    unittest.main()
