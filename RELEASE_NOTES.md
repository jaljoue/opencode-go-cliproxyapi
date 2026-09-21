## What's Changed

### Bug Fixes

- Normalize `role: "developer"` to `role: "system"` in the chat-completions adapter, preventing DeepSeek-backed models from rejecting valid client requests with HTTP 400 ([#5](https://github.com/massiveits/opencode-go-cliproxyapi/issues/5)). Thanks to [@zlwu](https://github.com/zlwu).

## Upgrade Notes

- Replace the old plugin binary with the new release binary.
- Restart CLIProxyAPI after replacing the plugin.
- Hard-refresh Management Center if the plugin page looks stale.

**Full Changelog**: https://github.com/massiveits/opencode-go-cliproxyapi/compare/v0.1.8...v0.1.9