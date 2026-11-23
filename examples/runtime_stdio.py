"""Run a turn through a bundled lune-core stdio process."""

import argparse
from collections import deque
import itertools
import json
from pathlib import Path
import subprocess
import uuid


def run(process, prompt, answer):
    requests = itertools.count(1)
    events = deque()

    def receive():
        line = process.stdout.readline()
        if not line:
            raise RuntimeError("lune-core closed its protocol output")
        return json.loads(line)

    def call(method, **params):
        request_id = next(requests)
        request = {"jsonrpc": "2.0", "id": request_id, "method": method, "params": params}
        process.stdin.write(json.dumps(request) + "\n")
        process.stdin.flush()
        while True:
            message = receive()
            if message.get("method") == "runtime.event":
                events.append(message["params"]["event"])
            elif message.get("id") == request_id:
                if "error" in message:
                    raise RuntimeError(message["error"])
                return message["result"]

    hello = call("runtime.hello", clientName="python-stdio-example")
    if hello["protocolVersion"] != "1":
        raise RuntimeError("unsupported lune-core protocol version")

    session_id = call("session.create")["id"]
    call("runtime.subscribe", sessionId=session_id, subscriptionId="example", afterCursor=0)
    execution_id = call(
        "turn.submit", sessionId=session_id, submissionId=str(uuid.uuid4()), input=prompt
    )["executionId"]
    while True:
        if events:
            event = events.popleft()
        else:
            message = receive()
            if message.get("method") != "runtime.event":
                continue
            event = message["params"]["event"]

        if event["kind"] == "approval_required":
            for pending in call("runtime.pending", sessionId=session_id) or []:
                if pending["kind"] == "approval":
                    call(
                        "approval.resolve", sessionId=session_id,
                        id=pending["approval"]["id"], decision="deny",
                    )
        if event["kind"] != "task_finished" or event.get("executionId") != execution_id:
            continue
        if event["status"] != "paused":
            return call("turn.wait", sessionId=session_id, executionId=execution_id)

        for pending in call("runtime.pending", sessionId=session_id) or []:
            if pending["kind"] == "user_question":
                question = pending["userQuestion"]
                answers = [
                    {"question_id": item["id"], "answer": answer}
                    for item in question["questions"]
                ]
                call(
                    "question.resolve", sessionId=session_id,
                    answer={
                        **{key: question[key] for key in ("executionId", "runId", "invocationId", "callId")},
                        "answers": answers,
                    },
                )
                break
        else:
            raise RuntimeError("turn paused; inspect runtime.pending before recovering")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("core", type=Path)
    parser.add_argument("workspace", type=Path)
    parser.add_argument("prompt")
    parser.add_argument("--answer", required=True, help="answer to use for a model clarification")
    args = parser.parse_args()
    process = subprocess.Popen(
        [str(args.core.resolve()), "stdio", "--workspace", str(args.workspace)],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, encoding="utf-8",
    )
    try:
        result = run(process, args.prompt, args.answer)
        process.stdin.close()
        if process.wait(timeout=35):
            raise RuntimeError("lune-core did not exit successfully")
        print(json.dumps(result))
    finally:
        if process.stdin and not process.stdin.closed:
            process.stdin.close()
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


if __name__ == "__main__":
    main()
