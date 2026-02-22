# AetherTunnel 系统架构图

## 📐 整体架构

```mermaid
graph TB
    subgraph "应用层 (Application Layer)"
        A1[Web Dashboard]
        A2[CLI Interface]
        A3[API Gateway]
    end

    subgraph "业务逻辑层 (Business Logic Layer)"
        B1[Proxy Manager]
        B2[Control Manager]
        B3[Audit Logger]
        B4[Connection Pool]
    end

    subgraph "协议层 (Protocol Layer)"
        C1[Message Protocol]
        C2[WebSocket Handler]
        C3[SCTP Handler]
        C4[HTTP/2 Handler]
    end

    subgraph "传输层 (Transport Layer)"
        D1[TLS 1.3]
        D2[yamux Multiplexing]
        D3[Obfuscation Layer]
    end

    subgraph "网络层 (Network Layer)"
        E1[TCP]
        E2[UDP]
        E3[QUIC]
        E4[IPv4/IPv6]
    end

    A1 --> B1
    A2 --> B1
    A3 --> B1
    B1 --> B2
    B2 --> C1
    C1 --> D1
    D1 --> D2
    D2 --> D3
    D3 --> E4
    E4 --> E1
    E4 --> E2
    E4 --> E3
    B3 -.-> B1
    B4 -.-> B1
```

## 🔧 分层架构

```mermaid
graph LR
    subgraph Layer1[应用层]
        A1[Dashboard UI]
        A2[Configuration UI]
        A3[Analytics UI]
    end

    subgraph Layer2[服务层]
        B1[Proxy Service]
        B2[Connection Service]
        B3[Audit Service]
        B4[Health Service]
    end

    subgraph Layer3[协议层]
        C1[Protocol Handler]
        C2[Message Encoder]
        C3[Message Decoder]
    end

    subgraph Layer4[传输层]
        D1[TLS Wrapper]
        D2[Multiplexer]
        D3[Obfuscator]
    end

    subgraph Layer5[网络层]
        E1[TCP/IP]
        E2[UDP/IP]
        E3[QUIC]
    end

    A1 --> B1
    A2 --> B1
    A3 --> B1
    B1 --> B2
    B2 --> B3
    B3 --> B4
    B4 --> C1
    C1 --> C2
    C2 --> C3
    C3 --> D1
    D1 --> D2
    D2 --> D3
    D3 --> E1
    E1 --> E2
    E1 --> E3
```

## 🏗️ 服务端架构

```mermaid
graph TB
    subgraph "AetherTunnel Server"
        S1[HTTP/HTTPS Proxy]
        S2[TCP Proxy]
        S3[UDP Proxy]
        S4[Dashboard Server]

        S5[Proxy Manager]
        S6[Control Manager]
        S7[Connection Pool]

        S8[Auth Manager]
        S9[Audit Logger]
        S10[Security Manager]
        S11[Stats Collector]

        S12[TLS Handler]
        S13[yamux Handler]
        S14[Obfuscation Layer]
    end

    S1 --> S5
    S2 --> S5
    S3 --> S5
    S4 --> S11

    S5 --> S6
    S6 --> S7

    S8 --> S6
    S9 --> S6
    S10 --> S6

    S6 --> S12
    S12 --> S13
    S13 --> S14

    S14 --> S11
    S11 --> S9
```

## 🌐 客户端架构

```mermaid
graph TB
    subgraph "AetherTunnel Client"
        C1[Proxy Client]
        C2[Control Client]
        C3[Connection Manager]

        C4[Auth Manager]
        C5[Heartbeat Handler]
        C6[Connection Pool]

        C7[TLS Client]
        C8[yamux Client]
        C9[Obfuscation Layer]
    end

    C1 --> C2
    C2 --> C3

    C4 --> C2
    C5 --> C2
    C6 --> C3

    C2 --> C7
    C7 --> C8
    C8 --> C9
```

## 🔄 通信流程

```mermaid
sequenceDiagram
    participant Client as Client
    participant CM as Control Manager
    participant PM as Proxy Manager
    participant TLS as TLS Handler
    participant Mux as Multiplexer
    participant Server as Server

    Client->>CM: 1. TCP/TLS Connect
    CM->>TLS: Establish Connection
    TLS->>Server: TLS Handshake
    Server-->>TLS: Handshake Complete
    TLS-->>CM: Connection Ready

    CM->>Mux: Create Multiplexed Channel
    Mux-->>CM: Channel ID

    Client->>CM: 2. Login Message
    CM->>TLS: Encrypt & Send
    TLS->>Server: Encrypted Message
    Server->>TLS: Decrypt Message
    TLS-->>CM: Decrypted Login

    CM->>Server: Verify Token & Signature
    Server-->>CM: Login Success

    CM->>PM: Register Proxy
    PM-->>CM: Proxy Config

    loop Heartbeat
        Client->>CM: Ping Message
        CM->>TLS: Encrypt & Send
        Server->>TLS: Decrypt & Send Pong
        TLS-->>CM: Pong Response
    end

    Client->>PM: Start Data Transfer
    PM->>Mux: Open Data Channel
    Mux-->>PM: Channel ID
    PM->>Server: Data Channel
    Server-->>PM: Data Forwarded
```

## 📊 数据流架构

```mermaid
graph LR
    subgraph "External Traffic"
        E1[User Request]
        E2[Firewall]
        E3[NAT]
    end

    subgraph "Server"
        S1[HTTP Proxy]
        S2[TCP Proxy]
        S3[UDP Proxy]

        S4[Proxy Manager]
        S5[Connection Pool]

        S6[TLS Handler]
        S7[Multiplexer]
        S8[Obfuscation]
    end

    subgraph "Client"
        C1[Connection Manager]
        C2[Proxy Client]

        C3[TLS Client]
        C4[Multiplexer]
        C5[Obfuscation]
    end

    subgraph "Local Services"
        L1[Web Server]
        L2[SSH Server]
        L3[Database]
    end

    E1->>E2
    E2->>E3
    E3->>S1
    S1->>S4
    S4->>S5
    S5->>S6
    S6->>S7
    S7->>S8
    S8->>C3
    C3->>C4
    C4->>C5
    C5->>C2
    C2->>C1
    C1->>L1
    C1->>L2
    C1->>L3
```

## 🌟 创新功能架构

```mermaid
graph TB
    subgraph "Core Tunnel"
        CT1[Protocol Handler]
        CT2[Transport Layer]
        CT3[Network Layer]
    end

    subgraph "Traffic Obfuscation"
        TO1[TLS Fake]
        TO2[HTTP Fake]
        TO3[XOR Obfuscator]
        TO4[Stream Encryptor]
    end

    subgraph "Adaptive Protocol"
        AP1[Network Monitor]
        AP2[Protocol Selector]
        AP3[Quality Scorer]
        AP4[Switch Manager]
    end

    subgraph "Smart Routing"
        SR1[Rule Engine]
        SR2[Health Checker]
        SR3[Load Balancer]
        SR4[Failover Manager]
    end

    subgraph "Visualization"
        VI1[Metrics Collector]
        VI2[Data Processor]
        VI3[Dashboard UI]
        VI4[Analytics Engine]
    end

    CT1 --> TO1
    CT1 --> TO2
    CT1 --> TO3
    CT1 --> TO4

    CT1 --> AP1
    AP1 --> AP2
    AP2 --> AP3
    AP3 --> AP4

    CT1 --> SR1
    SR1 --> SR2
    SR2 --> SR3
    SR3 --> SR4

    CT1 --> VI1
    VI1 --> VI2
    VI2 --> VI3
    VI3 --> VI4
```

## 🗂️ 模块依赖关系

```mermaid
graph TB
    subgraph "pkg/config"
        C1[Config Manager]
    end

    subgraph "pkg/crypto"
        CR1[Encryption]
        CR2[Signature]
    end

    subgraph "pkg/protocol"
        P1[Message Protocol]
    end

    subgraph "pkg/net"
        N1[TLS Handler]
        N2[Multiplexer]
        N3[Connection Wrapper]
    end

    subgraph "pkg/server"
        S1[Control Manager]
        S2[Proxy Manager]
        S3[Dashboard Server]
    end

    subgraph "pkg/vpn"
        V1[VPN Manager]
        V2[Tunnel Manager]
    end

    subgraph "pkg/obfuscation"
        O1[TLS Obfuscator]
        O2[HTTP Obfuscator]
    end

    subgraph "pkg/adaptive"
        A1[Protocol Monitor]
        A2[Quality Scorer]
    end

    subgraph "pkg/routing"
        R1[Rule Engine]
        R2[Load Balancer]
    end

    subgraph "pkg/visualization"
        VZ1[Metrics Collector]
        VZ2[Dashboard UI]
    end

    C1 --> CR1
    C1 --> P1
    CR1 --> N1
    P1 --> N2
    N1 --> S1
    N2 --> S2
    S1 --> S3
    S2 --> V1
    V1 --> V2
    N2 --> O1
    N2 --> O2
    N1 --> A1
    A1 --> A2
    N2 --> R1
    R1 --> R2
    N2 --> VZ1
    VZ1 --> VZ2
```

## 🚀 性能优化架构

```mermaid
graph LR
    subgraph "Connection Pool"
        CP1[Idle Connections]
        CP2[Active Connections]
        CP3[Connection Factory]
        CP4[Eviction Policy]
    end

    subgraph "Multiplexing"
        M1[Channel Pool]
        M2[Message Queue]
        M3[Zero-Copy Buffer]
    end

    subgraph "Data Path"
        D1[Read Buffer]
        D2[Process Buffer]
        D3[Write Buffer]
    end

    subgraph "Monitoring"
        MON1[Latency Monitor]
        MON2[Throughput Monitor]
        MON3[Error Rate Monitor]
    end

    CP3 --> CP1
    CP1 --> CP2
    CP2 --> CP3
    CP3 --> CP4

    CP2 --> M1
    M1 --> M2
    M2 --> M3

    M3 --> D1
    D1 --> D2
    D2 --> D3

    D3 --> MON1
    D3 --> MON2
    D3 --> MON3

    MON1 -.-> CP4
    MON2 -.-> M3
    MON3 -.-> M3
```

## 🛡️ 安全架构

```mermaid
graph TB
    subgraph "Security Layers"
        SL1[Network Security]
        SL2[Transport Security]
        SL3[Application Security]
        SL4[Data Security]
    end

    subgraph "Network"
        NS1[Firewall]
        NS2[NAT Traversal]
        NS3[DDoS Protection]
    end

    subgraph "Transport"
        TS1[TLS 1.3]
        TS2[Perfect Forward Secrecy]
        TS3[Certificate Validation]
    end

    subgraph "Application"
        AS1[Token Authentication]
        AS2[Ed25519 Signature]
        AS3[IP Whitelist]
        AS4[Rate Limiting]
        AS5[IP Ban]
    end

    subgraph "Data"
        DS1[ChaCha20-Poly1305]
        DS2[Nonce Protection]
        DS3[Message Authentication]
        DS4[Encryption Key Rotation]
    end

    SL1 --> NS1
    SL1 --> NS2
    SL1 --> NS3

    SL2 --> TS1
    SL2 --> TS2
    SL2 --> TS3

    SL3 --> AS1
    SL3 --> AS2
    SL3 --> AS3
    SL3 --> AS4
    SL3 --> AS5

    SL4 --> DS1
    SL4 --> DS2
    SL4 --> DS3
    SL4 --> DS4
```

## 📈 扩展性架构

```mermaid
graph TB
    subgraph "Core System"
        CS1[Proxy Interface]
        CS2[Protocol Interface]
        CS3[Transport Interface]
    end

    subgraph "Plugins"
        PL1[Auth Plugin]
        PL2[Encryption Plugin]
        PL3[Obfuscation Plugin]
        PL4[Routing Plugin]
        PL5[Analytics Plugin]
    end

    subgraph "Extensions"
        EX1[Custom Protocol]
        EX2[Custom Transport]
        EX3[Custom Proxy]
    end

    CS1 --> PL1
    CS1 --> PL2
    CS1 --> PL3
    CS1 --> PL4
    CS1 --> PL5

    CS2 --> EX1
    CS3 --> EX2
    CS1 --> EX3

    subgraph "Middleware"
        MW1[Logging Middleware]
        MW2[Metrics Middleware]
        MW3[Security Middleware]
    end

    CS1 -.-> MW1
    CS1 -.-> MW2
    CS1 -.-> MW3
```

---

**架构图版本**: v1.0.2
**最后更新**: 2026-02-23
**维护者**: AetherTunnel Team
