# pkg/config

TOML 配置的解析与校验：默认值、未知键警告、环境变量覆盖密钥、按角色校验；
`LoadString` 让嵌入方（`pkg/mobile`）直接从字符串装载同一套配置。

Parses and validates TOML configuration: defaults, unknown-key warnings,
environment overrides, per-role checks; `LoadString` lets embedders load the same
configuration from a string.
