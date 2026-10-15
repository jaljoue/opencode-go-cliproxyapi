## What's Changed

Full OpenAI Codex CLI support across all models: multi-agent namespaces, custom programmatic tools (`exec`), tool call results, and reasoning efforts are fully functional.

### New Models

- Add Gemini support through OpenCode Go's native Gemini API (`/v1/models/{model}:generateContent`), starting with `gemini-4-argon-contributor`. Chat Completions, Messages, and Responses clients are translated with CLIProxyAPI's built-in Gemini translators, including tool calls, thought signatures, and Codex custom tools. Gemini 4 Argon Contributor requires the workspace Privacy setting that allows paid endpoints that train on request data.
- Route `space-bunny`, `step-*`, and `cat-jam-*` models to Chat Completions.
- The default `base-url` is now `https://opencode.ai/inference/go/v1`, the only base that serves Gemini models. Configs that set `base-url` to `/zen/go/v1` keep working for the other protocols.

### Bug Fixes

- Fix missing Responses stream item lifecycle completion events (output_text.done, content_part.done, function_call_arguments.done, output_item.done) prior to response.completed, resolving dropped assistant output and tool calls in OpenAI Codex CLI and strict Responses clients.
- Fix handling of in-history messages with role: "system" across protocol adapters without rejecting them as unsupported roles or forwarding invalid turn roles to upstream providers that require alternating user/assistant turns.
- Fix handling of multi-part content arrays in function_call_output.output under /v1/responses decoding.
- Fix upstream HTTP >= 400 error propagation in streaming and non-streaming requests by reading and extracting the upstream error payload instead of discarding it with generic fallback errors.
- Add two-way Responses tool namespace and additional_tools translation: merge dynamic declarations from input items (type: "additional_tools"), unroll client-side grouping tools (type: "namespace") into qualified wire names for upstream models, and restore original tool names and namespaces across non-streaming output and streaming SSE events for OpenAI Codex CLI and multi-agent harnesses.
- Add Codex custom tool support on Chat Completions and Messages routes: custom tools (`type: "custom"`, e.g. `exec`, `apply_patch`) are declared as functions taking one `input` string, `custom_tool_call` history is replayed as function calls, and returned calls are restored as `custom_tool_call` items with `response.custom_tool_call_input.delta`/`.done` events.
- With `strip-hosted-web-search` enabled (the default), `tool_search` and `image_generation` are removed alongside hosted web search on translated routes.
- Remove local reasoning effort validation across request translators: allow client-declared reasoning effort levels (e.g. `xhigh`, `max`, and unlisted levels) to pass through transparently to upstream endpoints without local gatekeeping in `chatcompletions`, `responses`, and `messages` adapters.
- Normalize requests for non-GPT native Responses models (e.g. `muse-spark`, `grok`): unroll namespaces, convert custom tools and their history into function calls, sanitize `web_search` definitions, and drop historical `reasoning`/`compaction` items whose encrypted content is tied to another account. Flattened and custom calls are restored in responses. Native GPT models (e.g. `gpt-6-luna`) keep transparent passthrough.


## Upgrade Notes

- The plugin is now built against the CLIProxyAPI v8 SDK (v8.0.29, plugin ABI schema 6) and requires CLIProxyAPI v8. v7 hosts reject it.
- Replace the old plugin binary with the new release binary.
- Restart CLIProxyAPI after replacing the plugin.
- Hard-refresh Management Center if the plugin page looks stale.

**Full Changelog**: https://github.com/massiveits/opencode-go-cliproxyapi/compare/v0.1.9...v0.1.10