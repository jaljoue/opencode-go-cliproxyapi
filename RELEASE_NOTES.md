## What's Changed

- Fix HTTP 400 errors on DeepSeek models (`deepseek-flash`, `deepseek-v4.1-flash`, etc.) caused by OpenCode Go's backend rejecting untyped client reasoning metadata (such as `thinking: { levels: [...] }` sent by some OpenAI-compatible clients). Incoming `/v1/chat/completions` requests are now sanitized to strip untyped `thinking` metadata while preserving native `reasoning_effort` and valid Anthropic-style thinking objects.

## Upgrade Notes

- Replace the old plugin binary with the new release binary.
- Restart CLIProxyAPI after replacing the plugin.
- Hard-refresh Management Center if the plugin page looks stale.

**Full Changelog**: https://github.com/massiveits/opencode-go-cliproxyapi/compare/v0.1.5...v0.1.6
