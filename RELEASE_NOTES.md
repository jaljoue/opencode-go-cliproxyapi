## What's Changed

- Fix the OpenCode Go quota page on CLIProxyAPI v7.2.159+ where the new native `POST /v0/management/plugins/:id/quota` route intercepted the plugin's quota request before it reached the plugin. The plugin's quota data route is now `/plugins/opencode-go-cliproxyapi/quota-usage`; the Management Center menu and page URL are unchanged.
- Thanks to [@turnercore](https://github.com/turnercore) for the fix in [#3](https://github.com/massiveits/opencode-go-cliproxyapi/pull/3).

## Upgrade Notes

- Replace the old plugin binary with the new release binary.
- Restart CLIProxyAPI after replacing the plugin.
- Hard-refresh Management Center if the plugin page looks stale.

**Full Changelog**: https://github.com/massiveits/opencode-go-cliproxyapi/compare/v0.1.6...v0.1.7