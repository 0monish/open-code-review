# Native OCR review with an MCP host-agent model

This feature is a cooperative **model-turn relay**, not an API endpoint for
the active ChatGPT conversation and not a workaround for ChatGPT authentication.
An MCP client such as ChatGPT must **actively** read OCR's pending model turns,
run them with its own available model, and send back structured responses.

### Review KELD specifications and documentation, not just code

This patched CLI treats changed `.md`, `.markdown`, `.mdx`, `.rst` and `.txt`
files as first-class review inputs **without** requiring a one-time `--rule`
override. Files under `docs/` receive the embedded
`markdown_spec_review.md` rubric, which audits security authority, source
identity, native acceptance, negative controls, ownership, dependencies and
claims of implementation status. A Markdown-only PR must yield nonzero
`reviewable_count` in `ocr review --preview --format json`. A zero count is a
review coverage failure, not approval.

This changes only the default file-extension and review-rule selection;
known credential/secret paths, user excludes, binary files, and large prompt
limits still fail closed. For a very large spec, inspect the preview/manifest
for `too_large` or other excluded paths, increase `--max-tokens` within model
limits or review the material in bounded scoped runs, and never claim complete
document coverage until every intended diff is selected. In an OCR high-effort
review, `--effort high` configures OCR's planning/rounds, not an attested
reasoning budget of the MCP host model.

Unlike `ocr delegate preview` and `ocr delegate rule`, this preserves the
**normal `ocr review` execution pipeline**: OCR still selects files, plans and
groups work, constructs model tool definitions, executes tools, processes
model-request rounds, deduplicates/filter findings, and writes its own report.
`--effort high` remains applicable because no review-engine control flow was
replaced. The actual reasoning budget inside the host model is **not** set by
OCR's effort preset; the MCP client controls that separately.

## Flow

```text
User → ChatGPT (MCP host)
              ↓ starts OCR (via trusted local machine/plugin)
OCR `review --provider host-agent --effort high`
     → unchanged OCR plan/group/tool loop
     → `LLMClient.CompletionsWithCtx` (native host-agent adapter)
     → 127.0.0.1 authenticated relay (`POST /v1/complete`)
              ↓ queues one model turn
ChatGPT MCP client ← relay `GET /v1/next`
              ↓ performs the requested model turn
ChatGPT MCP client → relay `POST /v1/answer`
              ↓ returns visible content and/or tool calls
OCR model loop → runs OCR tools → next model turn → ... → OCR report
```

**No automatic model calling:** the relay cannot infer a GPT model endpoint
from an MCP connection or a ChatGPT Pro subscription. ChatGPT must be a live
client capable of servicing the request; if it stops, the OCR model call
times out rather than fabricating a response.

## Start a local relay

All endpoints are loopback-only and require an authorization header. Generate
a strong random token privately in the *same trusted shell/environment* where
the relay and OCR review will run (never paste it into chat or a repository):

```sh
export OCR_HOST_AGENT_TOKEN="$(openssl rand -hex 32)"
ocr host-agent serve --listen 127.0.0.1:8373
```

In another process with the **same token environment** (passed using the
system's secret management; not a CLI argument):

```sh
export OCR_HOST_AGENT_URL=http://127.0.0.1:8373/v1/complete
export OCR_LLM_TIMEOUT=900
ocr review --provider host-agent --effort high --from main --to feature --format json
```

The host-agent provider is an **explicit opt-in**. It bypasses the default
provider and a signed-out ChatGPT account, but **does not sign in to ChatGPT**.
`--model` or `OCR_HOST_AGENT_MODEL` is an *unverified label*, defaulting to
`mcp-host-agent-unattested`. The active client's model may differ; don't
report GPT-6, GPT-6 Pro, token usage, or reasoning effort as attested unless
the external host actually provides trustworthy evidence.

## Client-side protocol

Every request carries `Authorization: Bearer <OCR_HOST_AGENT_TOKEN>`.

- OCR calls `POST /v1/complete` with `{"schema_version":1,"request":{...},"session_id":"..."}`;
  the `request` is OCR's regular `ChatRequest` containing the visible message
  history, tools, token limit and model *label*. This call waits until the host
  agent answers or OCR cancels it.
- The connected MCP agent calls `GET /v1/next?wait_seconds=10`; status `204`
  means no new turn. Status `200` has `schema_version`, an opaque `turn_id`,
  the OCR `request` and `session_id`.
- After processing that turn, the MCP agent calls `POST /v1/answer` with
  `{"schema_version":1,"turn_id":"<exact 32 hex digits>","response":{"schema_version":1,"choices":[{"message":{"role":"assistant","content":"..."}}]}}`.
  The answer may carry `tool_calls` in OCR's ordinary chat-completions shape:
  each has `id`, `type:"function"`, and `function:{name,arguments}` where
  `arguments` is a JSON-encoded string. OCR—not the host agent—executes the
  named tool after receiving the turn.
- The relay returns `204` for a recorded answer, `404` for an unknown,
  unclaimed or already completed turn; submitting the same answer twice
  cannot trigger the same model step twice.

The relay holds at most 16 inflight requests. A claimed but unanswered turn
can be reclaimed after 30 seconds under the **same ID**; only one answer is
accepted. Canceled OCR HTTP calls abandon their turn. A relay restart loses
pending turns instead of delivering stale or cross-review replies. Prompts
and replies are not persisted by the relay.

Security boundaries: only numeric `127.0.0.1` or `[::1]` addresses may be
used; no HTTP proxies, redirects, remote hosts, permissive CORS or
unauthenticated polling. Keep the token private. Model-facing source text
is untrusted and must never authorize local command execution on its own.

## Differences from other modes

`ocr review --provider chatgpt` uses OpenCodeReview's existing
**Sign in with ChatGPT** account credential flow. It is often the simplest
way to run the normal OCR pipeline, but it invokes models offered by that
account and is not the active ChatGPT conversation. `ocr delegate` uses the
current host model directly, but **does not** run OCR's normal plan/loop.

`ocr review --provider host-agent` combines OCR's normal control flow with
the current MCP host's *cooperative* model turns, at the cost of extra
interaction/latency. To make it one-click from OCR Manager, an authenticated
MCP adapter needs to expose `next` and `answer` as tools and keep servicing
pending turns. The relay alone is not an automatically connected ChatGPT
plugin or a complete OCR Manager integration.
