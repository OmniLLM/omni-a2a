# Integrating an AI Agent with the Omni Agent Hub

This guide is for authors of **AI agents** (LLM tool-loop clients such as
`omni-pilot`) that want to use the Omni Agent Hub as a fan-out backend for
tools and skills provided by one or more upstream A2A agents.

If you are writing a plain non-agent HTTP client (a UI, a CLI wrapper, a
scripted caller), read [`client-integration-guide.md`](./client-integration-guide.md)
first. This document assumes you already understand the wire protocol and
focuses on the *design decisions* an agent must make when using the hub as a
tool.

---

## 1. Mental model

Your agent already has a tool loop: the model requests tool calls, your
runtime executes them, results feed back into the model, repeat. The hub
becomes **one tool** (or a small set of tools) in that loop. It multiplexes
many capabilities behind one endpoint:

```
                       ┌──────────────────────────┐
your model             │ Omni Agent Hub  :8222    │      upstream A (omnilauncher)
    │                  │                          ├────► plugin:tool:shell_exec
    │  tool-call:      │  POST /                  │      plugin:query:clipboard
    │  a2a_message_send│  JSON-RPC 2.0            │      launcher:query_all
    ├─────────────────►│                          │      skill:aws, skill:gcp, ...
    │◄─── result ──────┤  Bearer <api_key>        │
    │                  │                          ├────► upstream B (research)
                       └──────────────────────────┘      search, summarize, ...
```

The hub is deliberately thin. It **does not run an LLM itself**. It routes
one JSON-RPC message to whichever upstream owns the requested capability
and returns whatever that upstream produced. The upstream may be a pure
tool executor (calculator, shell) *or* another AI agent — in the
`omnilauncher` case, `skill:*` calls invoke OmniLauncher's own AI, which
runs a full tool loop of its own before answering.

This shape has two consequences your agent must plan for:

1. **Latency varies wildly.** A `plugin:tool:calculator` call returns in a
   few ms. A `skill:gcp` call may spend 5–60 s inside the upstream's own AI
   loop before responding. Do not treat every hub call as "fast".
2. **Some responses are conversational.** An upstream AI may return a
   *clarifying question* instead of a final answer. Your agent must either
   pass that question back to the top-level user or answer it itself before
   sending a follow-up.

---

## 2. Capability taxonomy — the two flavours

The hub advertises every upstream capability as a namespaced skill ID in its
composite Agent Card:

```
GET /.well-known/agent-card.json   (no auth)
```

Every ID looks like `<upstream>.<capability-id>`, e.g. `omnilauncher.skill:gcp`.
The **suffix** (after the first `.`) tells you which flavour you're calling:

| Suffix pattern | Flavour | Semantics |
|---|---|---|
| `plugin:tool:*` | **Direct tool** — structured call, typed args, returns typed output. Fast. | The hub forwards a `message/send` whose `data`-part is your JSON arguments; the upstream executes and returns. Think of it as a remote procedure call. |
| `plugin:query:*` | **Direct query plugin** — natural-language query, returns launcher-style results. Fast. | Send free text; get back a structured `query_results` artifact (title/subtitle/action per hit). |
| `launcher:query_all` | **Fan-out query** across every plugin on the upstream. Fast. | Cross-plugin autocomplete/search endpoint. |
| `skill:*` | **AI-mediated skill** — the upstream AI must decide *how* to execute. Slow. May be conversational. | The upstream loads the skill's SKILL.md, may call more tools internally, may ask a clarifying question. Behaves like sub-agent delegation. |

**Why this matters for your agent's system prompt / tool schema:** don't
expose all 75+ hub skills to the model as identical tools. Group them by
flavour and hint at their cost. A common pattern is:

- Register `plugin:tool:*` capabilities as individual, typed tools in your
  model's tool schema. The LLM picks the right one directly.
- Register `skill:*` capabilities as a **single meta-tool** in your schema
  ("delegate to the omni-agent-hub skill named X") — the model picks the
  skill *name*, and your runtime calls the hub. This keeps your tool count
  manageable and matches the fact that `skill:*` calls are conversational
  anyway.
- Consider not exposing `launcher:query_all` to the model at all unless the
  agent is specifically a launcher/autocomplete surface — its output shape
  is UI-oriented, not agent-oriented.

---

## 3. Authentication

Every `POST /` request needs the hub's client bearer key (from
`server.api_key` in the hub's `config.yaml`):

```
Authorization: Bearer <api_key>
```
or the equivalent `X-API-Key: <api_key>` header. Public endpoints
(`/health`, `/metrics`, `/.well-known/*`) accept no auth.

**Key management for an agent runtime:**
- Store the hub's api_key in the same secret store you use for LLM API keys.
- Rotating it means restarting the hub with new config *and* refreshing
  every agent runtime's copy — treat the two systems as tightly coupled.
- If you deploy the hub as a multi-tenant service later, plan now for
  per-tenant keys; the current single-key model works fine for a solo user
  or a trusted team, and less well past that.

---

## 4. The one wire format you need

All four operations are the same JSON-RPC 2.0 envelope, POSTed to `/`:

```json
{
  "jsonrpc": "2.0",
  "id": <your-request-id>,
  "method": "message/send",
  "params": { ... }
}
```

The method is always one of:

| Method | Purpose |
|---|---|
| `message/send` | Unary send + await final task. Use this for 95% of calls. |
| `message/sendSubscribe` | Same params, SSE-streamed intermediate states. Only if you want progress ticks. |
| `tasks/get` | Fetch a task by hub task id. Terminal tasks return cached; active tasks re-fetch from upstream. |
| `tasks/cancel` | Ask the upstream to cancel an in-flight task. |

### 4.1 Params shape

```json
{
  "message": {
    "messageId": "<your-message-id>",
    "role": "user",
    "parts": [
      // one of:
      {"type": "text", "text": "how many VMs in GCP?"},
      {"type": "data", "data": {"query": "how many VMs in GCP?", "region": "us-central1"}}
    ]
  },
  "contextId": "<opaque-string-you-choose>",   // optional but strongly recommended
  "skillId":   "omnilauncher.skill:gcp"        // optional; see §5
}
```

**Parts:**
- `type: "text"` is fine for conversational calls (`skill:*` and any request
  where you want the upstream AI to interpret free text).
- `type: "data"` is the right shape for structured `plugin:tool:*` calls —
  put the tool arguments directly in `data`. The hub preserves the full
  JSON object end-to-end (verified after a recent fix); unknown fields and
  future variants (`file`, `image`, ...) round-trip losslessly.
- You may include **multiple parts** in one message (e.g. a text prompt
  plus a structured data blob). Upstreams typically take the first text
  part as prose and the first data part as args.

### 4.2 Response shape (unary)

```json
{
  "jsonrpc": "2.0",
  "id": <echo of your request id>,
  "result": {
    "id": "<hub-task-id>",              // NOT the upstream's id — use for tasks/get and tasks/cancel
    "contextId": "<echo of your contextId, if any>",
    "status": {
      "state": "completed" | "failed" | "input-required" | "canceled" | "working" | "submitted",
      "message": {                       // present when state has an agent message attached
        "role": "agent",
        "parts": [{"type": "text", "text": "..."}]
      }
    },
    "artifacts": [                       // 0..N; each is a named collection of parts
      {
        "artifactId": "...",
        "name": "response",
        "parts": [
          {"type": "text", "text": "..."},
          {"type": "data", "data": {...}}
        ]
      }
    ],
    "history": [ /* echoed conversation turns */ ]
  }
}
```

Errors come back as standard JSON-RPC:
```json
{"jsonrpc": "2.0", "id": 1, "error": {"code": -32011, "message": "No route", "data": "..."}}
```

Error codes and how to react to them are covered in §9.

---

## 5. Routing — how the hub decides which upstream

The hub resolves the target upstream in this priority order (first match
wins):

1. **`contextId` stickiness** — if the `contextId` was used in a prior
   non-terminal task, the hub sends this turn to the same upstream, no
   matter what else is in the params.
2. **`skillId`** — if the id is namespaced (`<upstream>.<skill>`), the hub
   uses that upstream (as long as it's enabled). If it's un-namespaced but
   unambiguous (exactly one upstream advertises it), that works too.
3. **`@upstream-name` mention** at the start of the first text part — the
   hub strips it and forwards to that upstream.
4. **Upstream `prefix` string** at the start of the text — configured
   per-upstream in the hub's `config.yaml`.

For an agent client, **prefer `skillId`** in every request. It's explicit,
survives any prompt-injection attempt in user text, and lets the hub tell
you `-32601` up front instead of forwarding to a random default.

Include `contextId` on the *first* turn of any conversation you plan to
follow up on — you don't have to invent one, but if you don't, follow-ups
can't stick to the same upstream.

---

## 6. The agent tool-loop pattern

Here is the shape of using the hub from inside an LLM tool loop. Language
is illustrative — adapt to your runtime.

### 6.1 Startup

```python
class HubClient:
    def __init__(self, base_url: str, api_key: str, http_timeout_s: float = 310):
        self.base = base_url.rstrip("/")
        self.api_key = api_key
        # 310s > hub's own 300s upstream timeout, so we see hub errors
        # rather than local timeouts for slow skill:* calls.
        self.timeout = http_timeout_s

    def discover(self) -> dict:
        r = requests.get(f"{self.base}/.well-known/agent-card.json", timeout=10)
        r.raise_for_status()
        return r.json()   # {"skills": [{"id": "...", "name": "...", "description": "...", ...}, ...]}
```

Call `discover()` once at agent boot. Cache the result but **re-fetch every
few minutes** or when you see a `-32011 No route` — upstreams come and go
and the hub reshapes its composite card each time.

### 6.2 Deciding what to expose to the model

Given the composite card, split by flavour:

```python
def partition_skills(card):
    plugin_tools, skills, other = [], [], []
    for s in card.get("skills") or []:
        parts = s["id"].split(".", 1)
        if len(parts) != 2:
            continue
        _upstream, cap = parts
        if cap.startswith("plugin:tool:"):
            plugin_tools.append(s)
        elif cap.startswith("skill:"):
            skills.append(s)
        else:
            other.append(s)
    return plugin_tools, skills, other
```

- Register each `plugin:tool:*` as its own tool in the model's schema, using
  the skill's `inputSchema` (if present in the card entry) or fall back to
  a generic `{"query": string}` shape.
- Register `skills` collectively as one meta-tool:

```json
{
  "name": "invoke_skill",
  "description": "Delegate a task to a named skill on an upstream agent. Use for domain-specific workflows (cloud queries, formatting, external service lookups). The skill may respond with a clarifying question — treat that as needing more info from the user before you follow up.",
  "parameters": {
    "type": "object",
    "properties": {
      "skill_id": {"type": "string", "description": "Full skill id from the hub's agent card, e.g. omnilauncher.skill:gcp"},
      "query":    {"type": "string", "description": "Natural-language description of what you want the skill to do"},
      "context_id": {"type": "string", "description": "Reuse across turns of the same task; leave blank for a fresh conversation"}
    },
    "required": ["skill_id", "query"]
  }
}
```

### 6.3 Making a call

```python
def send(self, *, method="message/send", skill_id=None, text=None, data=None,
         context_id=None, message_id=None, request_id=None):
    parts = []
    if text is not None:
        parts.append({"type": "text", "text": text})
    if data is not None:
        parts.append({"type": "data", "data": data})
    if not parts:
        raise ValueError("send() needs at least one of text= or data=")

    body = {
        "jsonrpc": "2.0",
        "id": request_id or str(uuid.uuid4()),
        "method": method,
        "params": {
            "message": {
                "messageId": message_id or str(uuid.uuid4()),
                "role": "user",
                "parts": parts,
            },
        },
    }
    if skill_id:   body["params"]["skillId"] = skill_id
    if context_id: body["params"]["contextId"] = context_id

    r = requests.post(
        f"{self.base}/",
        headers={"Authorization": f"Bearer {self.api_key}", "Content-Type": "application/json"},
        json=body, timeout=self.timeout,
    )
    r.raise_for_status()
    return r.json()
```

### 6.4 Handling the result

Your tool-loop wrapper for a hub call is roughly:

```python
def run_hub_tool(self, skill_id: str, query: str, ctx: ConversationState) -> ToolResult:
    ctx_id = ctx.hub_context_id or (ctx.hub_context_id := str(uuid.uuid4()))
    resp = self.client.send(skill_id=skill_id, text=query, context_id=ctx_id)

    if "error" in resp:
        return self._handle_error(resp["error"], ctx)

    task = resp["result"]
    state = task["status"]["state"]

    match state:
        case "completed":
            return ToolResult.ok(self._flatten(task))
        case "input-required":
            # The upstream asked a clarifying question. Return the question
            # text to the model as the tool result; the model will either
            # answer it itself (send a follow-up on the same context_id) or
            # bubble the question up to the human user.
            return ToolResult.ok(
                self._flatten(task),
                hint="upstream requested more input — follow up on the same context_id",
            )
        case "failed":
            return ToolResult.error(self._flatten(task) or "upstream task failed")
        case "canceled":
            return ToolResult.error("task was canceled")
        case _:
            # "working" / "submitted" only appear for streaming or timeout;
            # for message/send the hub blocks until terminal, so seeing these
            # here is a bug — surface as error.
            return ToolResult.error(f"unexpected non-terminal state: {state}")

def _flatten(self, task) -> str:
    # Concatenate the agent's response message parts + any artifact text
    # parts. Data parts are kept as JSON blocks so the model can read them.
    chunks = []
    msg = (task.get("status") or {}).get("message") or {}
    for p in msg.get("parts") or []:
        chunks.append(p.get("text") or json.dumps(p.get("data")))
    for a in task.get("artifacts") or []:
        for p in a.get("parts") or []:
            chunks.append(p.get("text") or json.dumps(p.get("data")))
    return "\n".join(c for c in chunks if c)
```

**Why `input-required` matters.** With `skill:*` calls the upstream AI may
answer with a question rather than a result. Your agent's model should be
allowed to decide whether to answer autonomously or bubble the question up.
Treating `input-required` as "success — here's what the upstream said" is
usually what you want; the important thing is that you keep the same
`context_id` for the next call so the upstream continues the same task.

---

## 7. Multi-turn conversations

Rules that make multi-turn actually work:

1. **Pick a `contextId` per user conversation, not per tool call.** Reuse
   it across every hub call in that conversation. UUIDs work.
2. **Never mutate a `contextId` mid-conversation.** If you switch it, the
   upstream loses continuity.
3. **The hub returns the same hub task id across turns of the same
   context.** If you want to cancel or fetch by id later, that id is stable.
4. If you get `-32010 Unavailable` on a follow-up (breaker tripped for the
   sticky upstream), do **not** silently retry on a different upstream — the
   conversation state lives on the original one. Surface the error.

## 8. Timeouts and retries

- **Client HTTP timeout**: set to `≥ 310s` for `message/send`. The hub's
  own upstream timeout is 300s; you want to see the hub's error, not your
  local one.
- **`plugin:tool:*` calls** should complete in well under a second in
  practice; if one hangs, something upstream is broken — don't retry
  aggressively.
- **`skill:*` calls** invoke a whole LLM loop upstream and may legitimately
  take 5–60s. Show a loading indicator; do not retry idempotently.
- On any **network-level failure** (connection reset, TLS error), retry
  once with backoff; if it fails again, treat as terminal and surface it.
- On `-32010 Unavailable` (breaker open), back off 2–10s before retrying.
  The breaker auto-recovers on the first successful call, and now also on
  a successful card refresh once the exponential backoff window elapses
  (recent hub fix).

## 9. Error codes cheat sheet

| Code | Meaning | Your reaction |
|---|---|---|
| `-32700` Parse error | Malformed JSON leaving your client | Assert your serializer isn't emitting NaN / Inf / non-JSON |
| `-32600` Invalid request | Missing `jsonrpc:"2.0"` or `method` | Build the envelope with helpers, not string concat |
| `-32601` Method not found | Wrong method name | Only 4 valid: `message/send`, `message/sendSubscribe`, `tasks/get`, `tasks/cancel` |
| `-32602` Invalid params | Params don't match the method | Check the shapes in §4.1 |
| `-32001` Task not found | You called `tasks/get` / `tasks/cancel` with an id the hub doesn't know | Was it a hub task id? Not upstream id? Not truncated? |
| `-32002` Upstream HTTP error | Network / 5xx from the upstream | Retry once. If persistent, surface to user; the hub will mark the upstream unhealthy after 3 failures |
| `-32003` Invalid upstream response | Upstream returned non-JSON-RPC | Upstream bug; report to that team |
| `-32010` Unavailable | Breaker open — 3+ consecutive upstream failures, still inside the backoff window | Back off a few seconds and retry; ping `/health` in parallel to see recovery |
| `-32011` No route | The hub couldn't map your request to any upstream | Re-fetch the agent card; the skill may have been withdrawn |

Never treat `-32011` and `-32001` as retryable.

## 10. Streaming with SSE

Use `message/sendSubscribe` only if your UI wants live progress:

```
POST / HTTP/1.1
Content-Type: application/json
Authorization: Bearer <api_key>

{"jsonrpc":"2.0","id":1,"method":"message/sendSubscribe","params":{...}}
```

The connection upgrades to `text/event-stream` and the hub emits:

```
data: {"id":"<hub-task-id>","status":{"state":"working"},"final":false}

data: {"id":"<hub-task-id>","status":{"state":"working","message":{...}},"final":false}

data: {"id":"<hub-task-id>","status":{"state":"completed"},"final":true}
```

Terminate reading on `final: true` or on a terminal state
(`completed`/`failed`/`canceled`). If the upstream disconnects abnormally
the hub synthesizes a `state: failed` terminal event — you never need to
detect "hung" streams client-side.

Agent runtimes rarely benefit from streaming unless they're rendering
intermediate output to a user. If you don't render, use `message/send`.

## 11. Observability

- `GET /health` — no auth, returns upstream healthy/total counts. Good
  for a startup readiness probe.
- `GET /metrics` — Prometheus text. Includes
  `omni_a2a_upstream_healthy{upstream=...}` and
  `omni_a2a_upstream_consecutive_failures{upstream=...}` — worth scraping
  if you deploy multi-tenant.
- Hub server log: `~/.omni-agent-hub/logs/server.log` (JSON lines). Every
  request logs `trace_id` — put that on your client-side spans so you can
  correlate.

## 12. Implementation checklist

Copy this into your PR description when you land omni-pilot's hub client:

```
□ Client fetches /.well-known/agent-card.json on startup and refreshes every 5 min
□ Bearer token loaded from config/secret store; never logged
□ Send envelope is JSON-RPC 2.0, method one of the four allowed
□ Parts include "type" field; data parts use "type":"data","data": {...}
□ Every user-conversation-scoped call carries a stable contextId
□ Model tool schema splits plugin:tool:* (individual tools) from skill:* (one meta-tool)
□ Result parsing handles all six task states, not just "completed"
□ input-required routes back to the model as tool output with the question text
□ HTTP client timeout ≥ 310s
□ -32010 backoff, -32011 refresh-card-and-report, -32001/-32011 non-retryable
□ tasks/get and tasks/cancel use the hub task id (result.id), never any upstream id
□ trace_id (if you extract one) is stamped on client-side telemetry
```

## 13. See also

- [`client-integration-guide.md`](./client-integration-guide.md) — the
  low-level protocol reference. This document sits on top of it.
- Hub source: `internal/dispatch/dispatch.go` — the exact unary flow if you
  need to reason about corner cases.
- Router precedence: `internal/router/router.go` — for questions of the
  form "why did my request go to upstream X instead of Y".
