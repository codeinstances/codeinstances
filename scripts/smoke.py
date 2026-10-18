#!/usr/bin/env python3
# Copyright CodeInstances.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# The smoke check on built binaries, run by CI and by the release workflow on
# each system's binaries before anything is published.
#
#   python3 scripts/smoke.py BIN_DIR
#
# BIN_DIR holds codeinstances and codeinstances-demo (.exe on Windows). The
# check: both report the version in codeinstances.go; the demo passes every
# check (a Go program over HTTPS and HTTP with hooked clock and random values,
# replayed offline, changes caught, an edited file caught); and a Python
# program that isn't hooked at all is recorded against a live local server,
# the server stopped, and the run replayed with show and verify working on
# the file, diff telling it apart from a second run, test replaying it as a
# regression test, fork answering from the parent offline, group leaving
# passing runs out and grouping two crashes, a store served, pushed to and
# pulled from, a run signed and checked against trusted keys, and a store
# replaying that signed run on demand.
import http.server
import json
import os
import re
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
WINDOWS = os.name == "nt"


class Failure(Exception):
    pass


def require(ok, message):
    if not ok:
        raise Failure(message)


def run(args, timeout=120, check=True):
    result = subprocess.run([str(a) for a in args], capture_output=True, text=True, timeout=timeout)
    if check:
        require(result.returncode == 0, f"{Path(str(args[0])).name} {' '.join(str(a) for a in args[1:])} exited {result.returncode}:\n{result.stdout[-2000:]}\n{result.stderr[-4000:]}")
    return result


def source_version():
    found = re.findall(r'^const Version = "(.*)"$', (ROOT / "codeinstances.go").read_text(encoding="utf-8"), re.M)
    require(len(found) == 1, "codeinstances.go has no single Version constant")
    return found[0]


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"path": self.path, "at": time.time_ns()}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


PROGRAM = """
import json, urllib.request
for path in ("/one", "/two?x=1"):
    with urllib.request.urlopen(URL + path, timeout=20) as r:
        print(r.status, json.load(r)["at"])
"""


def smoke(bin_dir, version):
    suffix = ".exe" if WINDOWS else ""
    cli = Path(bin_dir).resolve() / ("codeinstances" + suffix)
    demo = Path(bin_dir).resolve() / ("codeinstances-demo" + suffix)
    for b in (cli, demo):
        require(b.exists(), f"{b} is missing")

    info = json.loads(run([cli, "version", "-json"]).stdout)
    require(info["version"] == version, f"codeinstances reports {info['version']}, the source says {version}")
    require(info["format"] == "codeinstances/0", f"format {info['format']}")
    print(f"ok   codeinstances {info['version']} ({info['os']}/{info['arch']}, commit {info['revision'][:12]})")

    rep = json.loads(run([demo, "-cli", cli, "-replays", "3"], timeout=300).stdout)
    require(rep["passed"] and rep["tool"]["version"] == version, "the demo failed: " + json.dumps([c for c in rep["checks"] if not c["passed"]]))
    print(f"ok   demo: {len(rep['checks'])} checks passed, {rep['calls']} calls, {rep['values']} values")

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    url = f"http://127.0.0.1:{server.server_address[1]}"
    with tempfile.TemporaryDirectory() as work:
        script = Path(work) / "program.py"
        script.write_text(f"URL = {url!r}\n" + PROGRAM, encoding="utf-8")
        file = Path(work) / "python.instance"
        rec = run([cli, "record", "-q", "-o", file, "--", sys.executable, script])
        require("2 calls" in rec.stderr, f"record: {rec.stderr}")
        second = Path(work) / "second.instance"
        run([cli, "record", "-q", "-o", second, "--", sys.executable, script])
        server.shutdown()
        server.server_close()
        rep = json.loads(run([cli, "replay", "-json", file]).stdout)
        require(rep["matched"] and rep["calls_served"] == 2, f"replay: {rep}")
        print("ok   an unhooked Python program replays offline")
        same = run([cli, "diff", file, file])
        require("the runs are the same" in same.stdout, f"diff of a file with itself: {same.stdout}")
        cmp = run([cli, "diff", "-json", file, second], check=False)
        require(cmp.returncode == 1, f"diff exited {cmp.returncode}: {cmp.stderr}")
        found = json.loads(cmp.stdout)
        require(found["calls"]["changed"] == 2 and any("response body at $.at: a " in d for d in found["differences"]), f"diff: {found}")
        print("ok   diff tells two runs apart, down to the JSON member")
        tested = run([cli, "test", file])
        require("1 of 1 tests passed" in tested.stdout, f"test: {tested.stdout}")
        print("ok   the Python run passes as a regression test")
        forked = Path(work) / "fork.instance"
        f = run([cli, "fork", "-q", "-offline", "-at", "2", "-o", forked, file], check=False)
        # The second request gets a 502, which urllib raises on: exit 1.
        require(f.returncode == 1 and "1 answered from the parent, 0 given, 0 sent, 1 not sent (offline)" in f.stderr, f"fork: {f.stderr}")
        require(json.loads(forked.read_text(encoding="utf-8"))["fork"]["at"] == 2, "fork: no fork member")
        print("ok   fork answers the first call from the parent and sends nothing offline")
        grouped = json.loads(run([cli, "group", "-json", file, second]).stdout)
        require(grouped["failing"] == 0 and grouped["passed"] == 2 and grouped["groups"] == [], f"group: {grouped}")
        print("ok   group counts the passing runs and groups none")
        crash = Path(work) / "crash.py"
        crash.write_text("import time\ndef load():\n    raise KeyError('row %d' % time.time_ns())\nload()\n", encoding="utf-8")
        for name in ("crash1.instance", "crash2.instance"):
            run([cli, "record", "-q", "-o", Path(work) / name, "--", sys.executable, crash], check=False)
        grouped = json.loads(run([cli, "group", "-json", Path(work) / "crash1.instance", Path(work) / "crash2.instance", file]).stdout)
        require(grouped["failing"] == 2 and len(grouped["groups"]) == 1 and grouped["groups"][0]["fingerprint"]["error"].startswith("KeyError: 'row <n>'\nat load (crash.py)"), f"group: {grouped}")
        print("ok   group puts two Python crashes with different messages in one group")
        server = subprocess.Popen([str(cli), "serve", "-dir", str(Path(work) / "store"), "-addr", "127.0.0.1:0", "-token", "smoke"],
                                  stderr=subprocess.PIPE, text=True)
        try:
            line = server.stderr.readline()
            require(" at http://127.0.0.1:" in line, f"serve: {line}")
            store = line.rsplit(" at ", 1)[1].strip()
            pushed = json.loads(run([cli, "push", "-json", "-to", store, "-token", "smoke", Path(work) / "crash1.instance"]).stdout)
            pid = pushed[0]["id"]
            pulled = Path(work) / "pulled.instance"
            run([cli, "pull", "-from", store, "-token", "smoke", "-o", pulled, pid.split(":")[1][:12]])
            require(pulled.read_bytes() == (Path(work) / "crash1.instance").read_bytes(), "pull gave a different file")
            report = run([cli, "report", "-url", store, pulled]).stdout
            require(report.startswith("## exit 1: `KeyError") and "codeinstances pull -from " + store in report, f"report: {report}")
        finally:
            server.terminate()
            server.wait(timeout=30)
        print("ok   serve, push, pull and report round trip")
        shown = run([cli, "show", file]).stdout
        require("GET " + url + "/two?x=1 -> 200" in shown, f"show: {shown}")
        require("intact" in run([cli, "verify", file]).stdout, "verify")
        key = Path(work) / "smoke"
        run([cli, "keygen", "-o", key, "-label", "smoke"])
        run([cli, "keygen", "-o", Path(work) / "other"])
        signed = Path(work) / "signed.instance"
        run([cli, "sign", "-key", str(key) + ".key", "-signer", "smoke", "-o", signed, second])
        trusted = run([cli, "verify", "-trust", str(key) + ".pub", signed]).stdout
        require("trusted as smoke" in trusted, f"verify -trust: {trusted}")
        require(run([cli, "verify", "-trust", Path(work) / "other.pub", signed], check=False).returncode == 1, "verify -trust took another key")
        require(run([cli, "verify", "-trust", str(key) + ".pub", second], check=False).returncode == 1, "verify -trust took an unsigned run")
        print("ok   keygen, sign and verify -trust")
        server = subprocess.Popen([str(cli), "serve", "-dir", str(Path(work) / "runs"), "-addr", "127.0.0.1:0", "-token", "smoke",
                                   "-run", "-trust", str(key) + ".pub", "-run-dir", work], stderr=subprocess.PIPE, text=True)
        try:
            line = server.stderr.readline()
            require(" at http://127.0.0.1:" in line, f"serve -run: {line}")
            store = line.rsplit(" at ", 1)[1].strip()
            run([cli, "push", "-to", store, "-token", "smoke", signed])
            sid = json.loads(signed.read_text(encoding="utf-8"))["id"]
            ran = run([cli, "run", "-on", store, "-token", "smoke", sid])
            require("on the store: matched" in ran.stdout, f"run: {ran.stdout}")
        finally:
            server.terminate()
            server.wait(timeout=30)
        print("ok   serve -run replays a signed run on demand")
        data = file.read_text(encoding="utf-8")
        file.write_text(data.replace('"exit_code": 0', '"exit_code": 1', 1), encoding="utf-8")
        require(run([cli, "verify", file], check=False).returncode == 1, "verify missed an edited file")
        print("ok   show, verify and an edited file")


def main():
    if len(sys.argv) != 2:
        print("usage: smoke.py BIN_DIR", file=sys.stderr)
        sys.exit(2)
    try:
        smoke(sys.argv[1], source_version())
    except (Failure, subprocess.TimeoutExpired, KeyError, ValueError) as e:
        print(f"FAIL {e}", file=sys.stderr)
        sys.exit(1)
    print("smoke check passed")


if __name__ == "__main__":
    main()
