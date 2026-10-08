# pkg/crypto

机密性所在的包：控制帧与隧道字节流的 AEAD（含可选的 X25519 + ML-KEM-768 后量子混合协商
与按流派生密钥）、Ed25519 身份与挑战应答、Schnorr 证明原语，以及常量时间比较。
它不负责伪装，那是 [`pkg/obfs`](../obfs)。

Where confidentiality lives: AEAD over control frames and tunnelled streams (with the
optional X25519 + ML-KEM-768 hybrid and per-stream keys), Ed25519 identities and
challenge primitives, the Schnorr proof primitives, and constant-time comparison. It
does not disguise anything — that is [`pkg/obfs`](../obfs).
