## What's Changed

### Bug Fixes
- Infer `type: "message"` for role-bearing Responses input items when `type` is omitted or empty, preventing valid client requests from failing with HTTP 500 ([#1](https://github.com/massiveits/opencode-go-cliproxyapi/issues/1)). Thanks to [@tangxijin](https://github.com/tangxijin).
- Return HTTP 400 Bad Request instead of status 0 for client-fault errors (`ClassUnsupported` and `ClassTranslation`) in envelope error responses, preventing CLIProxyAPI from misclassifying caller input errors as credential faults and placing the provider in cooldown ([#2](https://github.com/massiveits/opencode-go-cliproxyapi/issues/2)). Thanks to [@tangxijin](https://github.com/tangxijin).

## Upgrade Notes

- Replace the old plugin binary with the new release binary.
- Restart CLIProxyAPI after replacing the plugin.
- Hard-refresh Management Center if the plugin page looks stale.

**Full Changelog**: https://github.com/massiveits/opencode-go-cliproxyapi/compare/v0.1.7...v0.1.8