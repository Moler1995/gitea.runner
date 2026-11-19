#!/usr/bin/env python3
"""Local protocol fixture; it runs commands on the test host, without containers."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile

request = json.load(sys.stdin)
command = request["command"]
state = request["state"]
result = {}
error = None
trace = os.environ["GITEA_HOOK_TEST_ROOT"]

with open(Path(trace) / "events.jsonl", "a") as stream:
    stream.write(json.dumps({"jobId": request["jobId"], "command": command}) + "\n")

try:
    args = request["args"]
    if command == "prepare_job":
        root = Path(tempfile.mkdtemp(dir=trace))
        state = {"root": str(root)}
        paths = {key: str(root / key) for key in ("workspace", "act", "temp", "toolCache")}
        for directory in paths.values():
            Path(directory).mkdir()
        result = {"paths": paths, "runner": {"os": "Linux", "arch": "X64"},
                  "container": {"id": request["jobId"]},
                  "imageEnvironment": {"PATH": os.environ["PATH"]},
                  "services": {name: {"id": name, "ports": {}} for name in args.get("services", {})}}
        if os.environ.get("GITEA_HOOK_TEST_MODE") == "deny":
            error = {"code": "policy_denied", "message": "job was not approved"}
        print("prepare hook ready", flush=True)
    elif command == "copy_to_job":
        Path(args["destination"]).mkdir(parents=True, exist_ok=True)
        with tarfile.open(args["archiveFile"]) as archive:
            archive.extractall(args["destination"], filter="data")
    elif command == "copy_from_job":
        source = Path(args["source"])
        if not source.exists() and not source.is_symlink():
            error = {"code": "not_found", "message": "file does not exist"}
        else:
            with tarfile.open(args["archiveFile"], "w") as archive:
                if args["source"].endswith("/."):
                    for child in sorted(source.iterdir()):
                        archive.add(child, arcname=child.name)
                else:
                    archive.add(source, arcname=source.name)
    elif command in ("run_command_step", "run_container_step"):
        if "user" in args and args["user"]:
            raise RuntimeError("fixture does not support changing user")
        if args.get("dockerfile"):
            raise RuntimeError("fixture does not build images")
        env = dict(os.environ, **args.get("environmentVariables", {}))
        child = subprocess.run([args["entryPoint"], *args.get("entryPointArgs", [])],
                               cwd=args["workingDirectory"], env=env, check=False)
        result = {"exitCode": child.returncode if child.returncode >= 0 else 128 - child.returncode}
    elif command == "cleanup_job":
        if state.get("root"):
            shutil.rmtree(state["root"], ignore_errors=True)
        print("cleanup hook done", flush=True)
    else:
        raise RuntimeError("unknown command")
except Exception as exc:
    error = {"code": "hook_error", "message": str(exc)}

response = {"protocolVersion": 1, "requestId": request["requestId"], "state": state, "result": result}
if error:
    response["error"] = error
temporary = request["responseFile"] + ".tmp"
with open(temporary, "w") as stream:
    json.dump(response, stream)
os.replace(temporary, request["responseFile"])
