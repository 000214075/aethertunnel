# pkg/config

TOML 配置的解析与校验：默认值、未知键报告（从 frp 粘来的键会写出对应写法）、环境变量与
密钥文件覆盖、按角色校验，以及 `includes` 片段合并。`LoadString` 让嵌入方（`pkg/mobile`）
从字符串装载同一套配置，走同一条解析与校验路径。

Parses and validates TOML configuration: defaults, unknown-key reports (an frp key name
comes back with what to write instead), environment and key-file overrides, per-role
checks and `includes` merging. `LoadString` lets embedders load the same configuration
from a string through the same parse-and-validate path.
