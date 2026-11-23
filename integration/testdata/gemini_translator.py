#!/usr/bin/env python3
"""Answers the Gemini API on the workstation's loopback by asking OpenRouter.

Antigravity speaks the Gemini API alone, and OpenRouter does not: a test
that has Antigravity driven through OpenRouter starts this beside it, in
the same workstation, and names it to Antigravity as its endpoint. Every
request is answered with one model, whatever model it names. The key is
read from the environment and written nowhere.

    gemini_translator.py <port> <OpenRouter model>
"""

import json
import os
import sys
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

OPENROUTER = "https://openrouter.ai/api/v1/chat/completions"
MODEL = sys.argv[2]
KEY = os.environ["OPENROUTER_API_KEY"]

# What OpenRouter answered with for each call of a tool, by the call: a
# model's turn is sent back as it came, with what the model signed it by,
# and each call is answered under the ID it was made with.
answered = {}


def call_key(name, args):
    return name + " " + json.dumps(args or {}, sort_keys=True)


def schema(node):
    """A Gemini schema as JSON Schema: its types are written in capitals."""
    if isinstance(node, list):
        return [schema(n) for n in node]
    if not isinstance(node, dict):
        return node
    out = {}
    for key, value in node.items():
        if key == "type" and isinstance(value, str):
            out[key] = value.lower()
        elif key in ("properties", "$defs", "definitions") and isinstance(value, dict):
            out[key] = {name: schema(v) for name, v in value.items()}
        else:
            out[key] = schema(value)
    return out


def text_of(parts):
    return "".join(p.get("text", "") for p in parts or [] if "text" in p and not p.get("thought"))


def messages_of(body):
    messages = []
    system = text_of((body.get("systemInstruction") or {}).get("parts"))
    if system:
        messages.append({"role": "system", "content": system})
    pending = []  # the IDs of the calls not answered yet, with their names
    made = 0
    for content in body.get("contents", []):
        parts = content.get("parts") or []
        calls = [p["functionCall"] for p in parts if "functionCall" in p]
        results = [p["functionResponse"] for p in parts if "functionResponse" in p]
        # Antigravity sends a tool's results in a turn it calls the model's:
        # a turn is what its parts are, whatever it is called.
        if content.get("role") == "model" and not results:
            kept = answered.get(call_key(calls[0].get("name", ""), calls[0].get("args"))) if calls else None
            if kept is not None:
                message = kept
            else:
                message = {"role": "assistant", "content": text_of(parts)}
                if calls:
                    message["tool_calls"] = []
                    for call in calls:
                        made += 1
                        message["tool_calls"].append({
                            "id": call.get("id") or "call_%d" % made,
                            "type": "function",
                            "function": {"name": call.get("name", ""), "arguments": json.dumps(call.get("args") or {})},
                        })
            messages.append(message)
            pending = [(c["id"], c["function"]["name"]) for c in message.get("tool_calls") or []]
            continue
        for result in results:
            at = next((i for i, (call_id, _) in enumerate(pending) if call_id == result.get("id")), None)
            if at is None:
                at = next((i for i, (_, name) in enumerate(pending) if name == result.get("name")), 0 if pending else None)
            call_id = pending.pop(at)[0] if at is not None else result.get("id") or "call_unknown"
            messages.append({"role": "tool", "tool_call_id": call_id, "content": json.dumps(result.get("response"))})
        said = text_of(parts)
        if said or not results:
            messages.append({"role": "user", "content": said})
    return messages


def tools_of(body):
    tools = []
    for tool in body.get("tools") or []:
        for declared in tool.get("functionDeclarations") or []:
            parameters = declared.get("parametersJsonSchema") or schema(declared.get("parameters")) or {"type": "object", "properties": {}}
            tools.append({"type": "function", "function": {
                "name": declared.get("name", ""), "description": declared.get("description", ""), "parameters": parameters,
            }})
    return tools


def ask(body):
    request = {"model": MODEL, "messages": messages_of(body)}
    tools = tools_of(body)
    if tools:
        request["tools"] = tools
    limit = (body.get("generationConfig") or {}).get("maxOutputTokens")
    if limit:
        request["max_tokens"] = limit
    http = urllib.request.Request(OPENROUTER, json.dumps(request).encode(), {
        "Authorization": "Bearer " + KEY, "Content-Type": "application/json",
    })
    with urllib.request.urlopen(http, timeout=300) as response:
        answer = json.load(response)
    if "choices" not in answer:
        raise RuntimeError(json.dumps(answer.get("error", answer))[:500])
    message = answer["choices"][0]["message"]
    parts = []
    if message.get("content"):
        parts.append({"text": message["content"]})
    for call in message.get("tool_calls") or []:
        try:
            args = json.loads(call["function"].get("arguments") or "{}")
        except ValueError:
            args = {}
        answered[call_key(call["function"]["name"], args)] = message
        parts.append({"functionCall": {"name": call["function"]["name"], "args": args, "id": call["id"]}})
    usage = answer.get("usage") or {}
    return {
        "candidates": [{"content": {"role": "model", "parts": parts or [{"text": ""}]}, "finishReason": "STOP", "index": 0}],
        "usageMetadata": {
            "promptTokenCount": usage.get("prompt_tokens", 0),
            "candidatesTokenCount": usage.get("completion_tokens", 0),
            "totalTokenCount": usage.get("total_tokens", 0),
        },
        "modelVersion": MODEL,
    }


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def answer(self, status, payload, kind="application/json"):
        raw = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", kind)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        name = "models/" + MODEL
        model = {"name": name, "displayName": MODEL, "supportedGenerationMethods": ["generateContent", "countTokens"],
                 "inputTokenLimit": 1048576, "outputTokenLimit": 65536}
        self.answer(200, {"models": [model]} if self.path.split("?")[0].endswith("/models") else model)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        action = self.path.split("?")[0].rsplit(":", 1)[-1]
        if action == "countTokens":
            self.answer(200, {"totalTokens": len(json.dumps(body)) // 4})
            return
        if action not in ("generateContent", "streamGenerateContent"):
            self.answer(404, {"error": {"code": 404, "message": "the translator answers no " + action, "status": "NOT_FOUND"}})
            return
        try:
            result = ask(body)
        except urllib.error.HTTPError as err:
            said = err.read().decode(errors="replace")[:500]
            self.answer(err.code, {"error": {"code": err.code, "message": said, "status": "UNKNOWN"}})
            return
        except Exception as err:  # what went wrong is the agent's to show
            self.answer(502, {"error": {"code": 502, "message": str(err)[:500], "status": "UNAVAILABLE"}})
            return
        if action == "streamGenerateContent" and "alt=sse" in self.path:
            self.answer(200, b"data: " + json.dumps(result).encode() + b"\n\n", "text/event-stream")
        elif action == "streamGenerateContent":
            self.answer(200, [result])
        else:
            self.answer(200, result)


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
