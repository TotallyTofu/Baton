"""ComfyUI-Baton: ComfyUI jobs wait for the GPUs instead of grabbing them.

Before each queued job runs, this asks the baton daemon for the GPUs. Baton
first releases whatever holds them (TabbyAPI, Unsloth), or keeps the job
waiting while a game, a stream or a hold has them. The wait happens in
ComfyUI's worker thread, so the UI stays responsive and the job shows as
running; Cancel skips it.

If baton is not running, jobs run as they always did.
Set BATON_URL to reach a daemon on another address (default 127.0.0.1:9797).
"""

import json
import logging
import os
import threading
import time
import urllib.request

BATON_URL = os.environ.get("BATON_URL", "http://127.0.0.1:9797").rstrip("/")

NODE_CLASS_MAPPINGS = {}
NODE_DISPLAY_NAME_MAPPINGS = {}

log = logging.getLogger("comfyui-baton")
_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))  # never route loopback via a proxy


def _post(path, payload, timeout):
    req = urllib.request.Request(
        BATON_URL + path,
        data=json.dumps(payload).encode(),
        method="POST",
        headers={"Content-Type": "application/json", "X-Baton": "comfyui"},
    )
    with _opener.open(req, timeout=timeout) as r:
        return json.loads(r.read() or b"{}")


def _interrupted():
    try:
        import comfy.model_management as mm

        return mm.processing_interrupted()
    except Exception:
        return False


def acquire(prompt_id):
    """Block until baton grants the GPUs. False means the job was cancelled while waiting."""
    told = None
    errors = 0
    while True:
        try:
            r = _post("/api/gate/comfyui/acquire", {"prompt_id": prompt_id, "wait_ms": 5000}, timeout=20)
        except Exception as e:
            if told is None:
                log.info("[baton] daemon not reachable at %s (%s); running without it", BATON_URL, e)
            else:
                log.warning("[baton] lost the daemon while waiting (%s); running the job", e)
            return True
        if r.get("granted"):
            if told is not None:
                log.info("[baton] got the GPUs")
            return True
        if r.get("error"):
            errors += 1
            if errors >= 3:
                log.warning("[baton] %s; running the job anyway", r.get("reason"))
                return True
        reason = r.get("reason") or "waiting for the GPUs"
        if reason != told:
            log.info("[baton] job waiting: %s", reason)
            told = reason
        if _interrupted():
            log.info("[baton] cancelled while waiting for the GPUs")
            return False


def done(prompt_id):
    def run():
        try:
            _post("/api/gate/comfyui/done", {"prompt_id": prompt_id}, timeout=5)
        except Exception:
            pass

    threading.Thread(target=run, daemon=True).start()


def _skip(executor, prompt_id, extra_data):
    """Record a job cancelled at the gate the way ComfyUI records an interrupt."""
    executor.success = False
    executor.history_result = {"outputs": {}, "meta": {}}
    executor.status_messages = []
    try:
        executor.server.client_id = extra_data.get("client_id")
        executor.add_message(
            "execution_interrupted",
            {"prompt_id": prompt_id, "node_id": None, "node_type": None, "executed": [], "timestamp": int(time.time() * 1000)},
            broadcast=True,
        )
    except Exception:
        executor.status_messages = [("execution_interrupted", {"prompt_id": prompt_id})]
    try:
        import nodes

        nodes.interrupt_processing(False)
    except Exception:
        pass


def _install():
    import execution

    pe = execution.PromptExecutor
    if getattr(pe, "_baton_gate", False):
        return
    original = pe.execute

    def execute(self, prompt, prompt_id, extra_data={}, execute_outputs=[]):
        if not acquire(prompt_id):
            _skip(self, prompt_id, extra_data)
            done(prompt_id)
            return None
        try:
            return original(self, prompt, prompt_id, extra_data, execute_outputs)
        finally:
            done(prompt_id)

    pe.execute = execute
    pe._baton_gate = True
    log.info("[baton] gate installed: jobs ask %s for the GPUs before running", BATON_URL)


try:
    _install()
except Exception as e:  # never break ComfyUI over this
    log.warning("[baton] gate not installed: %s", e)
