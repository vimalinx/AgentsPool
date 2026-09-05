"""Dependency-free Python client for the AgentsPool HTTP API."""

from __future__ import annotations

import json
import mimetypes
import os
import ssl
import tempfile
import time
from pathlib import Path
from typing import Any, BinaryIO, Callable, Mapping, MutableMapping, Sequence
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlencode, urlsplit, urlunsplit
from urllib.request import Request, urlopen

JSON = dict[str, Any]
TERMINAL_TASK_STATUSES = {
    "succeeded",
    "failed",
    "cancelled",
    "dead_letter",
    "needs_review",
}


class AgentsPoolError(RuntimeError):
    """Base exception for SDK failures."""


class TransportError(AgentsPoolError):
    """Raised when the control plane cannot be reached."""


class APIError(AgentsPoolError):
    """Structured error returned by the AgentsPool control plane."""

    def __init__(self, status_code: int, code: str, message: str) -> None:
        self.status_code = status_code
        self.code = code
        self.message = message
        label = f" ({code})" if code else ""
        super().__init__(f"AgentsPool API returned HTTP {status_code}{label}: {message}")


class TaskTerminalError(AgentsPoolError):
    """Raised by wait_task when a task reaches a non-success terminal state."""

    def __init__(self, task: Mapping[str, Any]) -> None:
        self.task = dict(task)
        status = str(task.get("status", "unknown"))
        message = str(task.get("last_error") or "task did not succeed")
        super().__init__(f"task {task.get('id', '<unknown>')} finished as {status}: {message}")


class AgentsPoolClient:
    """Small synchronous client for the AgentsPool admin API.

    Parameters may also be supplied through ``AGENTSPOOL_SERVER`` and
    ``AGENTSPOOL_ADMIN_TOKEN``. The class is safe to reuse sequentially; it does
    not own background threads or persistent sockets.
    """

    def __init__(
        self,
        base_url: str | None = None,
        token: str | None = None,
        *,
        timeout: float = 30.0,
        user_agent: str = "agentspool-python/0.4.0a3",
        ssl_context: ssl.SSLContext | None = None,
    ) -> None:
        raw_url = (base_url or os.environ.get("AGENTSPOOL_SERVER") or "").strip()
        if not raw_url:
            raise ValueError("base_url is required (or set AGENTSPOOL_SERVER)")
        parts = urlsplit(raw_url.rstrip("/"))
        if parts.scheme not in {"http", "https"} or not parts.netloc:
            raise ValueError("base_url must be an absolute http:// or https:// URL")
        if timeout <= 0:
            raise ValueError("timeout must be positive")
        self._base_parts = parts
        self.token = token if token is not None else os.environ.get("AGENTSPOOL_ADMIN_TOKEN", "")
        self.timeout = float(timeout)
        self.user_agent = user_agent
        self.ssl_context = ssl_context

    def __enter__(self) -> "AgentsPoolClient":
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def close(self) -> None:
        """Compatibility hook; the dependency-free transport owns no session."""

    def status(self) -> JSON:
        return self._json("GET", "/api/v1/status")

    def list_nodes(self, *, status: str | None = None) -> list[JSON]:
        payload = self._json("GET", "/api/v1/nodes", query={"status": status})
        return _mapping_list(payload.get("nodes"), "nodes")

    def get_node(self, node_id: str) -> JSON:
        return self._json("GET", f"/api/v1/nodes/{_segment(node_id)}")["node"]

    def drain_node(self, node_id: str) -> JSON:
        return self._node_action(node_id, "drain")

    def enable_node(self, node_id: str) -> JSON:
        return self._node_action(node_id, "enable")

    def disable_node(self, node_id: str) -> JSON:
        return self._node_action(node_id, "disable")

    def delete_node(self, node_id: str) -> None:
        self._request("DELETE", f"/api/v1/nodes/{_segment(node_id)}", expect_json=False)

    def submit_task(self, spec: Mapping[str, Any]) -> JSON:
        if not isinstance(spec, Mapping):
            raise TypeError("spec must be a mapping")
        return self._json("POST", "/api/v1/tasks", json_body=dict(spec))

    def submit_task_file(self, path: str | os.PathLike[str]) -> JSON:
        with open(path, "r", encoding="utf-8") as handle:
            payload = json.load(handle)
        if not isinstance(payload, dict):
            raise ValueError("task JSON must contain an object")
        return self.submit_task(payload)

    def list_tasks(
        self,
        *,
        status: str | None = None,
        queue: str | None = None,
        limit: int = 100,
    ) -> list[JSON]:
        payload = self._json(
            "GET",
            "/api/v1/tasks",
            query={"status": status, "queue": queue, "limit": limit},
        )
        return _mapping_list(payload.get("tasks"), "tasks")

    def get_task(self, task_id: str) -> JSON:
        return self._json("GET", f"/api/v1/tasks/{_segment(task_id)}")

    def cancel_task(self, task_id: str) -> JSON:
        return self._json(
            "POST",
            f"/api/v1/tasks/{_segment(task_id)}/cancel",
            json_body={},
        )["task"]

    def retry_task(
        self,
        task_id: str,
        *,
        reset_attempts: bool = False,
        acknowledge_non_idempotent: bool = False,
    ) -> JSON:
        return self._json(
            "POST",
            f"/api/v1/tasks/{_segment(task_id)}/retry",
            json_body={
                "reset_attempts": reset_attempts,
                "acknowledge_non_idempotent": acknowledge_non_idempotent,
            },
        )["task"]

    def delete_task(self, task_id: str) -> JSON:
        """Delete a terminal task, its execution metadata, and execution logs."""

        return self._json("DELETE", f"/api/v1/tasks/{_segment(task_id)}")

    def task_logs(
        self,
        task_id: str,
        *,
        after: int = 0,
        limit: int = 1000,
        execution_id: str | None = None,
    ) -> JSON:
        return self._json(
            "GET",
            f"/api/v1/tasks/{_segment(task_id)}/logs",
            query={"after": after, "limit": limit, "execution_id": execution_id},
        )

    def execution_logs(
        self,
        execution_id: str,
        *,
        after: int = 0,
        limit: int = 1000,
    ) -> JSON:
        return self._json(
            "GET",
            f"/api/v1/executions/{_segment(execution_id)}/logs",
            query={"after": after, "limit": limit},
        )

    def wait_task(
        self,
        task_id: str,
        *,
        timeout: float | None = None,
        poll_interval: float = 1.0,
        on_log: Callable[[JSON], None] | None = None,
        raise_on_failure: bool = True,
    ) -> JSON:
        """Poll until terminal, optionally forwarding each new log event.

        Log cursors are tracked per execution. When a retry creates a new
        execution, the cursor resets so attempt 2 logs are not skipped because
        attempt 1 used a larger sequence number.
        """

        if timeout is not None and timeout <= 0:
            raise ValueError("timeout must be positive or None")
        if poll_interval <= 0:
            raise ValueError("poll_interval must be positive")
        deadline = None if timeout is None else time.monotonic() + timeout
        current_execution = ""
        log_after = 0

        while True:
            detail = self.get_task(task_id)
            task = detail["task"]
            executions = detail.get("executions") or []
            latest_execution = ""
            if executions:
                latest = max(
                    executions,
                    key=lambda item: (int(item.get("attempt", 0)), str(item.get("created_at", ""))),
                )
                latest_execution = str(latest.get("id", ""))

            if on_log is not None and latest_execution:
                if latest_execution != current_execution:
                    current_execution = latest_execution
                    log_after = 0
                logs = self.task_logs(
                    task_id,
                    after=log_after,
                    execution_id=current_execution,
                    limit=5000,
                )
                for event in logs.get("events", []):
                    if isinstance(event, dict):
                        on_log(event)
                log_after = int(logs.get("next_after", log_after))

            status = str(task.get("status", ""))
            if status in TERMINAL_TASK_STATUSES:
                if status != "succeeded" and raise_on_failure:
                    raise TaskTerminalError(task)
                return detail

            if deadline is not None:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError(f"timed out waiting for task {task_id}")
                time.sleep(min(poll_interval, remaining))
            else:
                time.sleep(poll_interval)

    def upload_artifact(
        self,
        file_path: str | os.PathLike[str],
        *,
        name: str | None = None,
        media_type: str | None = None,
    ) -> JSON:
        path = Path(file_path)
        if not path.is_file():
            raise FileNotFoundError(path)
        selected_name = name or path.name
        selected_media_type = media_type or mimetypes.guess_type(selected_name)[0] or "application/octet-stream"
        with path.open("rb") as handle:
            data = handle.read()
        return self._json(
            "POST",
            "/api/v1/artifacts",
            query={"name": selected_name, "media_type": selected_media_type},
            raw_body=data,
            headers={"Content-Type": selected_media_type},
        )["artifact"]

    def upload_artifact_stream(
        self,
        stream: BinaryIO,
        *,
        name: str,
        media_type: str = "application/octet-stream",
    ) -> JSON:
        """Upload a binary stream. The stream is buffered once by urllib."""

        data = stream.read()
        if not isinstance(data, bytes):
            raise TypeError("stream.read() must return bytes")
        return self._json(
            "POST",
            "/api/v1/artifacts",
            query={"name": name, "media_type": media_type},
            raw_body=data,
            headers={"Content-Type": media_type},
        )["artifact"]

    def list_artifacts(self, *, limit: int = 100) -> list[JSON]:
        payload = self._json("GET", "/api/v1/artifacts", query={"limit": limit})
        return _mapping_list(payload.get("artifacts"), "artifacts")

    def get_artifact(self, artifact_id: str) -> JSON:
        return self._json("GET", f"/api/v1/artifacts/{_segment(artifact_id)}")["artifact"]

    def delete_artifact(self, artifact_id: str, *, force: bool = False) -> JSON:
        """Delete Artifact metadata and unshared content.

        Without ``force``, the server refuses deletion while Tasks, Executions,
        or Sandbox Operations still reference the Artifact.
        """

        return self._json(
            "DELETE",
            f"/api/v1/artifacts/{_segment(artifact_id)}",
            query={"force": "true" if force else None},
        )

    def gc_artifacts(self, *, delete: bool = False) -> JSON:
        """Find orphan Artifact files; dry-run unless ``delete`` is true."""

        return self._json("POST", "/api/v1/artifacts/gc", json_body={"delete": delete})

    def download_artifact(
        self,
        artifact_id: str,
        destination: str | os.PathLike[str],
        *,
        overwrite: bool = False,
    ) -> Path:
        """Download to a temporary sibling, fsync, then atomically replace."""

        destination_path = Path(destination)
        if destination_path.exists() and not overwrite:
            raise FileExistsError(destination_path)
        destination_path.parent.mkdir(parents=True, exist_ok=True)
        payload = self._request(
            "GET",
            f"/api/v1/artifacts/{_segment(artifact_id)}/content",
            expect_json=False,
        )
        assert isinstance(payload, bytes)
        temporary_name = ""
        try:
            with tempfile.NamedTemporaryFile(
                mode="wb",
                prefix=f".{destination_path.name}.",
                suffix=".part",
                dir=destination_path.parent,
                delete=False,
            ) as handle:
                temporary_name = handle.name
                handle.write(payload)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary_name, destination_path)
            temporary_name = ""
        finally:
            if temporary_name:
                try:
                    os.unlink(temporary_name)
                except FileNotFoundError:
                    pass
        return destination_path

    def events(self, *, limit: int = 100, event_type: str | None = None) -> list[JSON]:
        payload = self._json(
            "GET",
            "/api/v1/events",
            query={"limit": limit, "type": event_type},
        )
        return _mapping_list(payload.get("events"), "events")

    def _node_action(self, node_id: str, action: str) -> JSON:
        return self._json(
            "POST",
            f"/api/v1/nodes/{_segment(node_id)}/{action}",
            json_body={},
        )["node"]

    def _json(
        self,
        method: str,
        path: str,
        *,
        query: Mapping[str, object | None] | None = None,
        json_body: Mapping[str, Any] | Sequence[Any] | None = None,
        raw_body: bytes | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> JSON:
        payload = self._request(
            method,
            path,
            query=query,
            json_body=json_body,
            raw_body=raw_body,
            headers=headers,
            expect_json=True,
        )
        if not isinstance(payload, dict):
            raise AgentsPoolError(f"expected JSON object from {path}")
        return payload

    def _request(
        self,
        method: str,
        path: str,
        *,
        query: Mapping[str, object | None] | None = None,
        json_body: Mapping[str, Any] | Sequence[Any] | None = None,
        raw_body: bytes | None = None,
        headers: Mapping[str, str] | None = None,
        expect_json: bool,
    ) -> JSON | list[Any] | bytes | None:
        if json_body is not None and raw_body is not None:
            raise ValueError("json_body and raw_body are mutually exclusive")
        request_headers: MutableMapping[str, str] = {
            "Accept": "application/json" if expect_json else "*/*",
            "User-Agent": self.user_agent,
        }
        if self.token:
            request_headers["Authorization"] = f"Bearer {self.token}"
        body = raw_body
        if json_body is not None:
            body = json.dumps(json_body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
            request_headers["Content-Type"] = "application/json"
        if headers:
            request_headers.update(headers)

        request = Request(
            self._url(path, query),
            data=body,
            headers=dict(request_headers),
            method=method.upper(),
        )
        try:
            with urlopen(request, timeout=self.timeout, context=self.ssl_context) as response:
                data = response.read()
                if not expect_json or not data:
                    return data if data else None
                return json.loads(data.decode("utf-8"))
        except HTTPError as error:
            try:
                data = error.read()
                code = ""
                message = error.reason or f"HTTP {error.code}"
                try:
                    envelope = json.loads(data.decode("utf-8"))
                    structured = envelope.get("error", {})
                    code = str(structured.get("code", ""))
                    message = str(structured.get("message", message))
                except (UnicodeDecodeError, json.JSONDecodeError, AttributeError):
                    text = data.decode("utf-8", errors="replace").strip()
                    if text:
                        message = text
            finally:
                error.close()
            raise APIError(error.code, code, message) from error
        except URLError as error:
            raise TransportError(f"request to {self._url(path, query)} failed: {error.reason}") from error
        except json.JSONDecodeError as error:
            raise AgentsPoolError(f"invalid JSON response from {path}: {error}") from error

    def _url(self, path: str, query: Mapping[str, object | None] | None) -> str:
        normalized_path = "/" + path.lstrip("/")
        base_path = self._base_parts.path.rstrip("/")
        encoded_query: dict[str, object] = {}
        if query:
            encoded_query = {key: value for key, value in query.items() if value is not None and value != ""}
        return urlunsplit(
            (
                self._base_parts.scheme,
                self._base_parts.netloc,
                base_path + normalized_path,
                urlencode(encoded_query, doseq=True),
                "",
            )
        )


def _segment(value: str) -> str:
    if not value or not value.strip():
        raise ValueError("resource ID must not be empty")
    return quote(value.strip(), safe="")


def _mapping_list(value: object, field: str) -> list[JSON]:
    if not isinstance(value, list):
        raise AgentsPoolError(f"response field {field!r} is not a list")
    result: list[JSON] = []
    for item in value:
        if not isinstance(item, dict):
            raise AgentsPoolError(f"response field {field!r} contains a non-object item")
        result.append(item)
    return result
