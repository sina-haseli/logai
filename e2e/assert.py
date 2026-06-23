#!/usr/bin/env python3
"""End-to-end assertions for the logai stack (stdlib only).

Drives the running docker-compose stack:
  1. Seeds an ERROR document into OpenSearch (poller path).
  2. Posts a buggy error to the webhook (deterministic path).
  3. Waits for each error to reach a terminal pipeline state.
  4. Asserts an MR was opened and the buggy file was actually changed.

Exit code 0 = all assertions passed.
"""
import json
import os
import sys
import time
import urllib.request
import urllib.error

# Windows consoles default to cp1252; force UTF-8 so status glyphs render.
try:
    sys.stdout.reconfigure(encoding="utf-8")
    sys.stderr.reconfigure(encoding="utf-8")
except Exception:
    pass

LOGAI = "http://localhost:3000"
OPENSEARCH = "http://localhost:9200"
GITLAB = "http://localhost:8080"

TERMINAL = {"fixed", "skipped", "failed"}


def req(method, url, body=None, headers=None, timeout=30):
    data = None
    h = {"Content-Type": "application/json"}
    if headers:
        h.update(headers)
    if body is not None:
        data = json.dumps(body).encode()
    r = urllib.request.Request(url, data=data, method=method, headers=h)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            raw = resp.read().decode()
            return resp.status, (json.loads(raw) if raw.strip() else None)
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, raw


def step(msg):
    print(f"\n=== {msg} ===", flush=True)


def fail(msg):
    print(f"\n❌ FAIL: {msg}", flush=True)
    sys.exit(1)


def ok(msg):
    print(f"✅ {msg}", flush=True)


def wait_terminal(error_id, label, timeout=240):
    print(f"   waiting for {label} ({error_id[:8]}) to finish...", flush=True)
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        s, body = req("GET", f"{LOGAI}/errors/{error_id}")
        if s == 200:
            status = body["error"]["status"]
            if status != last:
                stages = [f'{j["stage"]}:{j["status"]}' for j in body.get("jobs") or []]
                print(f"   status={status}  jobs=[{', '.join(stages)}]", flush=True)
                last = status
            if status in TERMINAL:
                return body
        time.sleep(3)
    fail(f"{label} did not reach a terminal state within {timeout}s (last={last})")


# ---------------------------------------------------------------------------
# 1. Seed OpenSearch
# ---------------------------------------------------------------------------
def seed_opensearch():
    step("Seed OpenSearch index with an ERROR document")
    mapping = {
        "mappings": {
            "properties": {
                "level": {"type": "keyword"},
                "message": {"type": "text"},
                "stack_trace": {"type": "text"},
                "service": {"type": "keyword"},
                "@timestamp": {"type": "date"},
            }
        }
    }
    # Create index (ignore "already exists").
    s, b = req("PUT", f"{OPENSEARCH}/logs-test", mapping)
    print(f"   create index -> {s}")

    doc = {
        "level": "ERROR",
        "message": "panic: runtime error: integer divide by zero (nightly batch)",
        "stack_trace": (
            "panic: runtime error: integer divide by zero\n\n"
            "goroutine 17 [running]:\n"
            "app.Divide(0x5, 0x0)\n"
            "\t/src/app/calculator.go:5\n"
            "app.RunBatch()\n"
            "\t/src/app/batch.go:22\n"
        ),
        "service": "calculator-service",
        "@timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    s, b = req("POST", f"{OPENSEARCH}/logs-test/_doc?refresh=true", doc)
    if s not in (200, 201):
        fail(f"could not index seed doc: {s} {b}")
    ok("OpenSearch seeded (1 ERROR doc)")


# ---------------------------------------------------------------------------
# 2. Webhook ingest (deterministic primary path)
# ---------------------------------------------------------------------------
def webhook_ingest():
    step("Ingest a buggy error via POST /webhook/error")
    # Unique nonce keeps each run a fresh fingerprint (avoids dedup on re-run).
    nonce = os.getenv("LOGAI_NONCE") or str(int(time.time()))
    payload = {
        "message": f"panic: runtime error: integer divide by zero [run {nonce}]",
        "stack_trace": (
            "panic: runtime error: integer divide by zero\n\n"
            "goroutine 1 [running]:\n"
            "app.Divide(0xa, 0x0)\n"
            "\t/src/app/calculator.go:5\n"
            "main.main()\n"
            "\t/src/main.go:12 +0x18\n"
        ),
        "service": "calculator-service",
        "severity": "error",
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    s, b = req("POST", f"{LOGAI}/webhook/error", payload)
    if s != 202 or "error_id" not in (b or {}):
        fail(f"webhook did not accept error: {s} {b}")
    eid = b["error_id"]
    ok(f"webhook accepted -> error_id={eid}")
    return eid


# ---------------------------------------------------------------------------
# Assertions on a fixed error
# ---------------------------------------------------------------------------
def assert_fixed_and_mr(error_id):
    body = wait_terminal(error_id, "webhook error")
    status = body["error"]["status"]
    if status != "fixed":
        # Print the failing job for diagnosis.
        for j in body.get("jobs") or []:
            if j["status"] == "failed":
                print(f"   failed stage {j['stage']}: {j['error_message']}")
        fail(f"expected webhook error to be 'fixed', got '{status}'")
    ok("pipeline reached status=fixed")

    # Show triage + localize + fix results pulled from the job rows.
    for j in body.get("jobs") or []:
        if j.get("result_json"):
            try:
                r = json.loads(j["result_json"])
                compact = {k: r[k] for k in r if k != "fixed_code"}
                print(f"   [{j['stage']}] {json.dumps(compact)}")
            except Exception:
                pass

    mr = body.get("merge_request")
    if not mr or not mr.get("branch_name"):
        fail("no merge_request attached to the fixed error")
    ok(f"MR recorded: branch={mr['branch_name']} url={mr.get('gitlab_mr_url')}")

    # /mrs endpoint should list it too.
    s, mrs = req("GET", f"{LOGAI}/mrs")
    if s != 200 or mrs.get("count", 0) < 1:
        fail(f"/mrs did not list the MR: {s} {mrs}")
    ok(f"/mrs lists {mrs['count']} merge request(s)")

    # Confirm GitLab side actually received an MR.
    s, gmrs = req("GET", f"{GITLAB}/_debug/mrs")
    if s != 200 or not gmrs:
        fail("mock GitLab has no MRs")
    print(f"   GitLab MR title: {gmrs[-1]['title']}")
    ok(f"mock GitLab recorded {len(gmrs)} MR(s)")

    # Verify the committed file on the fix branch actually changed.
    branch = mr["branch_name"]
    # Find the file path from the localize job.
    file_path = None
    for j in body.get("jobs") or []:
        if j["stage"] == "localize" and j.get("result_json"):
            file_path = json.loads(j["result_json"]).get("file_path")
    if not file_path:
        fail("could not determine localized file_path")

    s, original = req("GET", f"{GITLAB}/_debug/file?branch=main&path={file_path}")
    s2, fixed = req("GET", f"{GITLAB}/_debug/file?branch={branch}&path={file_path}")
    if s2 != 200:
        fail(f"committed file not found on branch {branch}: {s2} {fixed}")
    if original.get("content") == fixed.get("content"):
        fail("committed file is identical to the original (no real change)")
    ok(f"file {file_path} changed on branch {branch}")
    print("\n----- ORIGINAL -----")
    print(original.get("content"))
    print("----- AI FIX -----")
    print(fixed.get("content"))
    print("--------------------")


def assert_opensearch_ingested(timeout=90):
    step("Verify OpenSearch poller ingested the seeded error")
    deadline = time.time() + timeout
    while time.time() < deadline:
        s, body = req("GET", f"{LOGAI}/errors?limit=50")
        if s == 200:
            os_errs = [e for e in body.get("errors") or [] if e["source"] == "opensearch"]
            if os_errs:
                eid = os_errs[0]["id"]
                ok(f"poller ingested error {eid[:8]} (source=opensearch)")
                return eid
        time.sleep(5)
    fail(f"OpenSearch poller did not ingest the seeded error within {timeout}s")


def main():
    skip_os = os.getenv("SKIP_OPENSEARCH") == "1"

    if not skip_os:
        seed_opensearch()

    eid = webhook_ingest()
    assert_fixed_and_mr(eid)

    if not skip_os:
        os_eid = assert_opensearch_ingested()
        # Let the opensearch-sourced one finish too (best-effort, shorter wait).
        step("Wait for OpenSearch-sourced error to finish")
        body = wait_terminal(os_eid, "opensearch error", timeout=240)
        ok(f"opensearch error terminal status={body['error']['status']}")
    else:
        print("\n(skipping OpenSearch assertions: SKIP_OPENSEARCH=1)")

    print("\n🎉 ALL E2E ASSERTIONS PASSED")


if __name__ == "__main__":
    main()
