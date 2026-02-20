package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// ============================================================================
// 服务端配置结构
// ============================================================================

// ServerConfig 服务端完整配置
type ServerConfig struct {
	Server         ServerSection         `toml:"server"`
	TLS            TLSConfig              `toml:"tls"`
	AdvancedCrypto AdvancedCryptoConfig   `toml:"advanced_crypto"`
	Security       SecurityConfig         `toml:"security"`
	Logging        LoggingConfig          `toml:"logging"`
	Transport      TransportConfig        `toml:"transport"`
	LoadBalancer   LoadBalancerConfig     `toml:"load_balancer"`
	Monitoring     MonitoringConfig       `toml:"monitoring"`
	Database       DatabaseConfig         `toml:"database"`
	Plugins        PluginsConfig          `toml:"plugins"`
	Dashboard      DashboardConfig        `toml:"dashboard"`
	Proxy          ProxySection           `toml:"proxy"`
	HTTP           HTTPConfig             `toml:"http"`
	Session        SessionConfig          `toml:"session"`
	Network        NetworkConfig          `toml:"network"`
	Backup         BackupConfig           `toml:"backup"`
	Failover       FailoverConfig         `toml:"failover"`
	Notification   NotificationConfig     `toml:"notification"`
	Performance    PerformanceConfig       `toml:"performance"`
	CertManager    CertManagerConfig      `toml:"cert_manager"`
	Compliance     ComplianceConfig       `toml:"compliance"`
}

// ServerSection 服务端基础配置
type ServerSection struct {
	BindAddr                 string `toml:"bind_addr"`
	BindPort                 int    `toml:"bind_port"`
	AuthToken                string `toml:"auth_token"`
	VhostHTTPPort            int    `toml:"vhost_http_port"`
	VhostHTTPSPort           int    `toml:"vhost_https_port"`
	QuicEnabled              bool   `toml:"quic_enabled"`
	QuicPort                 int    `toml:"quic_port"`
	MaxConnections           int    `toml:"max_connections"`
	GracefulShutdownTimeout  int    `toml:"graceful_shutdown_timeout"`
	WorkerThreads            int    `toml:"worker_threads"`
}

// AdvancedCryptoConfig 现代加密配置
type AdvancedCryptoConfig struct {
	EnableEd25519            bool          `toml:"enable_ed25519"`
	EnableChaCha20Poly1305   bool          `toml:"enable_chacha20_poly1305"`
	KDFType                  string        `toml:"kdf_type"`
	Argon2idTime             int           `toml:"argon2id_time"`
	Argon2idMemory           int           `toml:"argon2id_memory"`
	Argon2idThreads          int           `toml:"argon2id_threads"`
	Argon2idKeylen           int           `toml:"argon2id_keylen"`
	KeyRotationInterval      time.Duration `toml:"key_rotation_interval"`
}

// LoggingConfig 日志配置
type LoggingConfig struct {
	Level            string   `toml:"level"`
	Format           string   `toml:"format"`
	Output           string   `toml:"output"`
	MaxSize          string   `toml:"max_size"`
	MaxAge           int      `toml:"max_age"`
	MaxBackups       int      `toml:"max_backups"`
	Compress         bool     `toml:"compress"`
	ConsoleOutput    bool     `toml:"console_output"`
	LogRequestBody   bool     `toml:"log_request_body"`
	LogResponseBody  bool     `toml:"log_response_body"`
	SensitiveFields  []string `toml:"sensitive_fields"`
}

// LoadBalancerConfig 负载均衡配置
type LoadBalancerConfig struct {
	Enabled               bool          `toml:"enabled"`
	Algorithm             string        `toml:"algorithm"`
	HealthCheckInterval   time.Duration `toml:"health_check_interval"`
	HealthCheckTimeout    time.Duration `toml:"health_check_timeout"`
	MaxFailures           int           `toml:"max_failures"`
	Backends              []BackendConfig `toml:"backends"`
}

// BackendConfig 后端节点配置
type BackendConfig struct {
	Name      string `toml:"name"`
	Addr      string `toml:"addr"`
	Weight    int    `toml:"weight"`
	MaxConns  int    `toml:"max_conns"`
}

// MonitoringConfig 监控配置
type MonitoringConfig struct {
	PrometheusEnabled        bool   `toml:"prometheus_enabled"`
	PrometheusPort           int    `toml:"prometheus_port"`
	PrometheusPath           string `toml:"prometheus_path"`
	OtelEnabled              bool   `toml:"otel_enabled"`
	OtelEndpoint             string `toml:"otel_endpoint"`
	OtelSampleRate           float64 `toml:"otel_sample_rate"`
	PprofEnabled             bool   `toml:"pprof_enabled"`
	PprofPort                int    `toml:"pprof_port"`
	ConnectionStats          bool   `toml:"connection_stats"`
	StatsInterval            int    `toml:"stats_interval"`
	CustomMetricsExporter    string `toml:"custom_metrics_exporter"`
	CustomMetricsEndpoint    string `toml:"custom_metrics_endpoint"`
}

// DatabaseConfig 数据库配置
type DatabaseConfig struct {
	Type            string        `toml:"type"`
	Host            string        `toml:"host"`
	Port            int           `toml:"port"`
	Username        string        `toml:"username"`
	Password        string        `toml:"password"`
	Database        string        `toml:"database"`
	RedisAddr       string        `toml:"redis_addr"`
	RedisPassword   string        `toml:"redis_password"`
	RedisDB         int           `toml:"redis_db"`
	MaxOpenConns    int           `toml:"max_open_conns"`
	MaxIdleConns    int           `toml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `toml:"conn_max_lifetime"`
}

// PluginsConfig 插件配置
type PluginsConfig struct {
	PluginDir    string                 `toml:"plugin_dir"`
	EnabledPlugins []string             `toml:"enabled_plugins"`
	PluginOptions map[string]map[string]interface{} `toml:"-"` // 动态插件配置
}

// HTTPConfig HTTP 特定配置
type HTTPConfig struct {
	ForceHTTPS               bool                 `toml:"force_https"`
	EnableHSTS               bool                 `toml:"enable_hsts"`
	HSTSMaxAge               int                  `toml:"hsts_max_age"`
	HSTSIncludeSubdomains    bool                 `toml:"hsts_include_subdomains"`
	EnableRateLimit          bool                 `toml:"enable_rate_limit"`
	RateLimitPerIP           int                  `toml:"rate_limit_per_ip"`
	MaxRequestBodySize       int                  `toml:"max_request_body_size"`
	EnableGzip              bool                 `toml:"enable_gzip"`
	GzipLevel               int                  `toml:"gzip_level"`
	CustomHeaders            []CustomHeaderConfig `toml:"custom_headers"`
	CORS                     CORSConfig            `toml:"cors"`
}

// CustomHeaderConfig 自定义响应头
type CustomHeaderConfig struct {
	Name  string `toml:"name"`
	Value string `toml:"value"`
}

// CORSConfig CORS 配置
type CORSConfig struct {
	Enabled           bool     `toml:"enabled"`
	AllowedOrigins    []string `toml:"allowed_origins"`
	AllowedMethods    []string `toml:"allowed_methods"`
	AllowedHeaders    []string `toml:"allowed_headers"`
	ExposedHeaders    []string `toml:"exposed_headers"`
	AllowCredentials  bool     `toml:"allow_credentials"`
	MaxAge            int      `toml:"max_age"`
}

// SessionConfig 会话管理配置
type SessionConfig struct {
	StoreType         string        `toml:"store_type"`
	Expiry            time.Duration `toml:"expiry"`
	MaxSessions       int           `toml:"max_sessions"`
	CleanupInterval   time.Duration `toml:"cleanup_interval"`
	RedisAddr         string        `toml:"redis_addr"`
	RedisPassword     string        `toml:"redis_password"`
	RedisDB           int           `toml:"redis_db"`
	RedisPrefix       string        `toml:"redis_prefix"`
}

// NetworkConfig 网络配置
type NetworkConfig struct {
	EnableReuseAddr      bool `toml:"enable_reuse_addr"`
	EnableKeepalive      bool `toml:"enable_keepalive"`
	TCPUserTimeout       int  `toml:"tcp_user_timeout"`
	RecvBufferSize       int  `toml:"recv_buffer_size"`
	SendBufferSize       int  `toml:"send_buffer_size"`
	EnableDeferAccept    bool `toml:"enable_defer_accept"`
	FastOpenQueue        int  `toml:"fast_open_queue"`
	EnableZeroCopy       bool `toml:"enable_zero_copy"`
}

// BackupConfig 备份配置
type BackupConfig struct {
	Enabled       bool   `toml:"enabled"`
	Interval      int    `toml:"interval"`
	BackupDir     string `toml:"backup_dir"`
	MaxBackups    int    `toml:"max_backups"`
	Compress      bool   `toml:"compress"`
	RetentionDays int    `toml:"retention_days"`
}

// FailoverConfig 故障转移配置
type FailoverConfig struct {
	Enabled           bool     `toml:"enabled"`
	PrimaryAddr       string   `toml:"primary_addr"`
	SecondaryAddrs    []string `toml:"secondary_addrs"`
	HeartbeatInterval int      `toml:"heartbeat_interval"`
	TimeoutThreshold  int      `toml:"timeout_threshold"`
	SwitchDelay       int      `toml:"switch_delay"`
}

// NotificationConfig 通知配置
type NotificationConfig struct {
	Enabled    bool                   `toml:"enabled"`
	Email      EmailNotification      `toml:"email"`
	Slack      SlackNotification      `toml:"slack"`
	Telegram   TelegramNotification   `toml:"telegram"`
	Webhook    WebhookNotification    `toml:"webhook"`
	Rules      []NotificationRule     `toml:"rules"`
}

// EmailNotification 邮件通知
type EmailNotification struct {
	Enabled    bool     `toml:"enabled"`
	SMTPServer string   `toml:"smtp_server"`
	SMTPUsername string `toml:"smtp_username"`
	SMTPPassword string `toml:"smtp_password"`
	From       string   `toml:"from"`
	To         []string `toml:"to"`
}

// SlackNotification Slack 通知
type SlackNotification struct {
	Enabled    bool   `toml:"enabled"`
	WebhookURL string `toml:"webhook_url"`
	Channel    string `toml:"channel"`
	Username   string `toml:"username"`
}

// TelegramNotification Telegram 通知
type TelegramNotification struct {
	Enabled  bool   `toml:"enabled"`
	BotToken string `toml:"bot_token"`
	ChatID   string `toml:"chat_id"`
}

// WebhookNotification Webhook 通知
type WebhookNotification struct {
	Enabled bool              `toml:"enabled"`
	URL     string            `toml:"url"`
	Method  string            `toml:"method"`
	Headers map[string]string `toml:"headers"`
}

// NotificationRule 通知规则
type NotificationRule struct {
	Name     string `toml:"name"`
	Enabled  bool   `toml:"enabled"`
	Threshold int   `toml:"threshold"`
	Window   string `toml:"window"`
	Severity string `toml:"severity"`
}

// PerformanceConfig 性能配置
type PerformanceConfig struct {
	EnableConnectionReuse bool          `toml:"enable_connection_reuse"`
	MaxReuseCount         int           `toml:"max_reuse_count"`
	EnableBatchSend       bool          `toml:"enable_batch_send"`
	BatchSize             int           `toml:"batch_size"`
	BatchTimeout          time.Duration `toml:"batch_timeout"`
	EnableMemoryPool      bool          `toml:"enable_memory_pool"`
	PoolSize              int           `toml:"pool_size"`
	EnableCPUAffinity     bool          `toml:"enable_cpu_affinity"`
	CPUCores              []int         `toml:"cpu_cores"`
	EnableHugePages       bool          `toml:"enable_huge_pages"`
}

// CertManagerConfig 证书管理配置
type CertManagerConfig struct {
	Enabled        bool   `toml:"enabled"`
	Email          string `toml:"email"`
	CacheDir       string `toml:"cache_dir"`
	CA             string `toml:"ca"`
	DNSProvider    string `toml:"dns_provider"`
	CloudflareAPIToken string `toml:"cloudflare_api_token"`
	CloudflareZoneID    string `toml:"cloudflare_zone_id"`
	RenewBefore    int    `toml:"renew_before"`
}

// ComplianceConfig 合规配置
type ComplianceConfig struct {
	EnableGDPR        bool     `toml:"enable_gdpr"`
	DataRetentionDays int      `toml:"data_retention_days"`
	EnableLogAnalysis bool     `toml:"enable_log_analysis"`
	AnomalyThreshold float64  `toml:"anomaly_threshold"`
	GenerateReports   bool     `toml:"generate_reports"`
	ReportInterval    string   `toml:"report_interval"`
	ReportRecipients  []string `toml:"report_recipients"`
}

// ============================================================================
// 客户端配置结构
// ============================================================================

// ClientConfig 客户端完整配置
type ClientConfig struct {
	Client         ClientSection        `toml:"client"`
	TLS            TLSConfig            `toml:"tls"`
	Transport      TransportConfig      `toml:"transport"`
	Reconnect      ReconnectConfig      `toml:"reconnect"`
	Proxy          ProxyConfigSection   `toml:"proxy"`
	ProxyDefaults  ProxyDefaultsConfig  `toml:"proxy_defaults"`
	Logging        LoggingConfig        `toml:"logging"`
	Network        ClientNetworkConfig  `toml:"network"`
	Cache          CacheConfig          `toml:"cache"`
	Plugins        PluginsConfig        `toml:"plugins"`
	Performance    PerformanceConfig    `toml:"performance"`
	Statistics     StatisticsConfig     `toml:"statistics"`
	Security       ClientSecurityConfig `toml:"security"`
	HealthCheck    ClientHealthCheck    `toml:"health_check"`
	Backup         BackupConfig         `toml:"backup"`
	Update         UpdateConfig         `toml:"update"`
	Failover       ClientFailoverConfig `toml:"failover"`
	Notification   NotificationConfig   `toml:"notification"`
	Proxies        []ProxyConfig        `toml:"proxies"`
}

// ClientSection 客户端基础配置
type ClientSection struct {
	ServerAddr      string `toml:"server_addr"`
	ServerPort      int    `toml:"server_port"`
	AuthToken       string `toml:"auth_token"`
	User            string `toml:"user"`
	ClientID        string `toml:"client_id"`
	ClientVersion   string `toml:"client_version"`
	PoolCount       int    `toml:"pool_count"`
	TLSServerName   string `toml:"tls_server_name"`
	Protocol        string `toml:"protocol"`
}

// ReconnectConfig 重连策略配置
type ReconnectConfig struct {
	Enabled               bool          `toml:"enabled"`
	MaxAttempts           int           `toml:"max_attempts"`
	Strategy              string        `toml:"strategy"`
	FixedInterval         time.Duration `toml:"fixed_interval"`
	ExponentialBase       int           `toml:"exponential_base"`
	ExponentialMax        time.Duration `toml:"exponential_max"`
	LinearIncrement       time.Duration `toml:"linear_increment"`
	Jitter                float64       `toml:"jitter"`
	ResetOnSuccess        bool          `toml:"reset_on_success"`
}

// ProxyConfigSection 客户端代理服务器配置
type ProxyConfigSection struct {
	Enabled        bool   `toml:"enabled"`
	ProxyType      string `toml:"proxy_type"`
	ProxyAddr      string `toml:"proxy_addr"`
	ProxyUsername  string `toml:"proxy_username"`
	ProxyPassword  string `toml:"proxy_password"`
	ProxyLocal     bool   `toml:"proxy_local"`
	ProxyTimeout   int    `toml:"proxy_timeout"`
}

// ClientNetworkConfig 客户端网络配置
type ClientNetworkConfig struct {
	BindInterface  string   `toml:"bind_interface"`
	DefaultGateway string   `toml:"default_gateway"`
	DNSServers     []string `toml:"dns_servers"`
	EnableIPv6     bool     `toml:"enable_ipv6"`
	MTU            int      `toml:"mtu"`
	BindAddr       string   `toml:"bind_addr"`
}

// CacheConfig 缓存配置
type CacheConfig struct {
	Enabled      bool   `toml:"enabled"`
	CacheDir     string `toml:"cache_dir"`
	MaxSize      int    `toml:"max_size"`
	TTL          int    `toml:"ttl"`
	Strategy     string `toml:"strategy"`
}

// StatisticsConfig 统计配置
type StatisticsConfig struct {
	Enabled       bool    `toml:"enabled"`
	Interval      int     `toml:"interval"`
	StatsFile     string  `toml:"stats_file"`
	Realtime      bool    `toml:"realtime"`
	ExportFormat  string  `toml:"export_format"`
	Prometheus    PrometheusStatsConfig `toml:"prometheus"`
	InfluxDB      InfluxDBStatsConfig   `toml:"influxdb"`
}

// PrometheusStatsConfig Prometheus 统计
type PrometheusStatsConfig struct {
	Enabled bool   `toml:"enabled"`
	Port    int    `toml:"port"`
	Path    string `toml:"path"`
}

// InfluxDBStatsConfig InfluxDB 统计
type InfluxDBStatsConfig struct {
	Enabled  bool   `toml:"enabled"`
	URL       string `toml:"url"`
	Database string `toml:"database"`
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// ClientSecurityConfig 客户端安全配置
type ClientSecurityConfig struct {
	EnableIPWhitelist   bool     `toml:"enable_ip_whitelist"`
	AllowedIPs          []string `toml:"allowed_ips"`
	MaxConnections      int      `toml:"max_connections"`
	MaxConnectionsPerIP int      `toml:"max_connections_per_ip"`
	SlowAttackThreshold int      `toml:"slow_attack_threshold"`
	RateLimit           int      `toml:"rate_limit"`
	EnableFingerprint   bool     `toml:"enable_fingerprint"`
}

// ClientHealthCheck 客户端健康检查
type ClientHealthCheck struct {
	Enabled bool             `toml:"enabled"`
	Interval time.Duration    `toml:"interval"`
	ServerHealthURL string    `toml:"server_health_url"`
	Checks    []HealthCheckItem `toml:"checks"`
}

// HealthCheckItem 健康检查项
type HealthCheckItem struct {
	Name           string `toml:"name"`
	Type           string `toml:"type"`
	Addr           string `toml:"addr"`
	URL            string `toml:"url"`
	Timeout        string `toml:"timeout"`
	ExpectedStatus int    `toml:"expected_status"`
	ExpectedBody   string `toml:"expected_body"`
}

// UpdateConfig 自动更新配置
type UpdateConfig struct {
	Enabled        bool   `toml:"enabled"`
	CheckInterval  int    `toml:"check_interval"`
	UpdateServer   string `toml:"update_server"`
	AutoDownload   bool   `toml:"auto_download"`
	AutoInstall    bool   `toml:"auto_install"`
	Prerelease     bool   `toml:"prerelease"`
}

// ClientFailoverConfig 客户端故障转移配置
type ClientFailoverConfig struct {
	Enabled        bool                   `toml:"enabled"`
	BackupServers   []BackupServerConfig   `toml:"backup_servers"`
	CheckInterval   int                    `toml:"check_interval"`
	SwitchDelay     int                    `toml:"switch_delay"`
	NotifyOnSwitch  bool                   `toml:"notify_on_switch"`
}

// BackupServerConfig 备用服务器配置
type BackupServerConfig struct {
	Addr     string `toml:"addr"`
	Priority int    `toml:"priority"`
}

// ProxyDefaultsConfig 全局代理默认配置
type ProxyDefaultsConfig struct {
	DefaultEncryption        bool               `toml:"default_encryption"`
	DefaultCompression       bool               `toml:"default_compression"`
	DefaultBandwidthLimit    string             `toml:"default_bandwidth_limit"`
	DefaultConnectionTimeout int                `toml:"default_connection_timeout"`
	DefaultReadTimeout      int                `toml:"default_read_timeout"`
	DefaultWriteTimeout     int                `toml:"default_write_timeout"`
	HealthCheck             HealthCheckConfig  `toml:"health_check"`
}

// ============================================================================
// 公共配置结构
// ============================================================================

// TLSConfig TLS 配置
type TLSConfig struct {
	Enabled         bool     `toml:"enabled"`
	CertFile        string   `toml:"cert_file"`
	KeyFile         string   `toml:"key_file"`
	CAFile          string   `toml:"ca_file"`
	ClientAuth      bool     `toml:"client_auth"`
	MinVersion      string   `toml:"min_version"`
	CipherSuites    []string `toml:"cipher_suites"`
	SessionTicketKey string   `toml:"session_ticket_key"`
	OCSPStapling    bool     `toml:"ocsp_stapling"`
	OCSPResponseFile string  `toml:"ocsp_response_file"`
	EnableSessionCache bool   `toml:"enable_session_cache"`
	SessionCacheSize int     `toml:"session_cache_size"`
	ALPNProtocols   []string `toml:"alpn_protocols"`
	ServerName      string   `toml:"server_name"`
	SkipVerify      bool     `toml:"skip_verify"`
}

// SecurityConfig 安全配置
type SecurityConfig struct {
	EnableIPWhitelist      bool     `toml:"enable_ip_whitelist"`
	AllowedIPs             []string `toml:"allowed_ips"`
	EnableIPBlacklist      bool     `toml:"enable_ip_blacklist"`
	BlockedIPs             []string `toml:"blocked_ips"`
	EnableGeoBlocking      bool     `toml:"enable_geo_blocking"`
	BlockedCountries       []string `toml:"blocked_countries"`
	AllowedCountries       []string `toml:"allowed_countries"`
	MaxConnectionsPerClient int      `toml:"max_connections_per_client"`
	MaxProxiesPerClient     int      `toml:"max_proxies_per_client"`
	HeartbeatTimeout        int      `toml:"heartbeat_timeout"`
	ConnectionTimeout        int      `toml:"connection_timeout"`
	ReadTimeout             int      `toml:"read_timeout"`
	WriteTimeout            int      `toml:"write_timeout"`
	EnableAuditLog          bool     `toml:"enable_audit_log"`
	AuditLogFile            string   `toml:"audit_log_file"`
	AuditLogMaxSize         string   `toml:"audit_log_max_size"`
	AuditLogMaxAge          int      `toml:"audit_log_max_age"`
	AuditLogMaxBackups      int      `toml:"audit_log_max_backups"`
	RateLimit               int      `toml:"rate_limit"`
	MaxFailedAttempts       int      `toml:"max_failed_attempts"`
	BlockDuration           time.Duration `toml:"block_duration"`
	EnableFingerprint       bool     `toml:"enable_fingerprint"`
	EnableSignature         bool     `toml:"enable_signature"`
	AntiReplayWindow        int      `toml:"anti_replay_window"`
}

// TransportConfig 传输配置
type TransportConfig struct {
	TCPMux                  bool `toml:"tcp_mux"`
	TCPMuxKeepaliveInterval int  `toml:"tcp_mux_keepalive_interval"`
	TCPKeepAlive            int  `toml:"tcp_keepalive"`
	MaxPoolCount            int  `toml:"max_pool_count"`
	MinPoolSize             int  `toml:"min_pool_size"`
	PoolMaxIdleTime         int  `toml:"pool_max_idle_time"`
	PoolHealthCheck         bool `toml:"pool_health_check"`
	PoolHealthCheckInterval int  `toml:"pool_health_check_interval"`
	EnableNagle             bool `toml:"enable_nagle"`
	EnableFastOpen          bool `toml:"enable_fast_open"`
	EnableReusePort         bool `toml:"enable_reuse_port"`
}

// DashboardConfig 仪表板配置
type DashboardConfig struct {
	Enabled           bool   `toml:"enabled"`
	Port              int    `toml:"port"`
	BindAddr          string `toml:"bind_addr"`
	Username          string `toml:"username"`
	Password          string `toml:"password"`
	AssetsDir         string `toml:"assets_dir"`
	EnableThemes      bool   `toml:"enable_themes"`
	DefaultTheme      string `toml:"default_theme"`
	EnableWebSocket   bool   `toml:"enable_websocket"`
	SessionTimeout    int    `toml:"session_timeout"`
	EnableAPIKey      bool   `toml:"enable_api_key"`
	APIKeys           []string `toml:"api_keys"`
}

// ProxySection 服务端代理配置
type ProxySection struct {
	BindAddr               string `toml:"bind_addr"`
	AllowPorts             string `toml:"allow_ports"`
	DenyPorts              string `toml:"deny_ports"`
	EnablePortReuse        bool   `toml:"enable_port_reuse"`
	DefaultEncryption      bool   `toml:"default_encryption"`
	DefaultCompression     bool   `toml:"default_compression"`
	DefaultBandwidthLimit  string `toml:"default_bandwidth_limit"`
}

// ProxyConfig 代理配置（客户端详细配置）
type ProxyConfig struct {
	Name               string                   `toml:"name"`
	Type               string                   `toml:"type"`
	LocalIP            string                   `toml:"local_ip"`
	LocalPort          int                      `toml:"local_port"`
	LocalSocketPath    string                   `toml:"local_socket_path"`
	RemotePort         int                      `toml:"remote_port"`
	UseEncryption      bool                     `toml:"use_encryption"`
	UseCompression     bool                     `toml:"use_compression"`
	BandwidthLimit     string                   `toml:"bandwidth_limit"`
	BandwidthLimitMode string                   `toml:"bandwidth_limit_mode"`
	ConnectionTimeout  int                      `toml:"connection_timeout"`
	Group              string                   `toml:"group"`
	GroupKey           string                   `toml:"group_key"`
	HealthCheck        HealthCheckConfig        `toml:"health_check"`
	Metas              map[string]string        `toml:"metas"`
	CustomDomains      []string                 `toml:"custom_domains"`
	SubDomain          string                   `toml:"subdomain"`
	Locations          []string                 `toml:"locations"`
	HTTPUser           string                   `toml:"http_user"`
	HTTPPwd            string                   `toml:"http_pwd"`
	HostHeaderRewrite  string                   `toml:"host_header_rewrite"`
	ForceHTTPS         bool                     `toml:"force_https"`
	TLS                ProxyTLSConfig           `toml:"tls"`
	HSTS               ProxyHSTSConfig          `toml:"hsts"`
	Pool               ProxyPoolConfig          `toml:"pool"`
	SQLWhitelist       SQLWhitelistConfig       `toml:"sql_whitelist"`
	ACL                ACLConfig                `toml:"acl"`
	NAT                NATConfig                `toml:"nat"`
	FallbackToRelay    bool                     `toml:"fallback_to_relay"`
	UDPTimeout         int                      `toml:"udp_timeout"`
	EnableReassembly   bool                     `toml:"enable_reassembly"`
	MaxReassemblySize  int                      `toml:"max_reassembly_size"`
	Connection         ProxyConnectionConfig     `toml:"connection"`
	Retry              RetryConfig              `toml:"retry"`
	CircuitBreaker     CircuitBreakerConfig     `toml:"circuit_breaker"`
	RateLimit          ProxyRateLimitConfig     `toml:"rate_limit"`
	AccessLog          AccessLogConfig          `toml:"access_log"`
	RequestTransform   []TransformConfig        `toml:"request_transform"`
	ResponseTransform  []TransformConfig        `toml:"response_transform"`
	SFTP               SFTPConfig               `toml:"sftp"`
	RDP                RDPConfig                `toml:"rdp"`
	WebSocket          WebSocketConfig         `toml:"websocket"`
	Chain              ChainConfig              `toml:"chain"`
	StaticFiles        StaticFilesConfig        `toml:"static_files"`
	SocketMode         string                   `toml:"socket_mode"`
	SocketUID          int                      `toml:"socket_uid"`
	SocketGID          int                      `toml:"socket_gid"`
}

// HealthCheckConfig 健康检查配置
type HealthCheckConfig struct {
	Type           string        `toml:"type"`
	Interval       time.Duration `toml:"interval"`
	Timeout        time.Duration `toml:"timeout"`
	MaxFailed      int           `toml:"max_failed"`
	URLOrPath      string        `toml:"url_or_path"`
	ExpectedStatus int           `toml:"expected_status"`
	ExpectedBody   string        `toml:"expected_body"`
	Headers        map[string]string `toml:"headers"`
}

// ProxyTLSConfig 代理 TLS 配置
type ProxyTLSConfig struct {
	Enabled     bool   `toml:"enabled"`
	SkipVerify  bool   `toml:"skip_verify"`
	ServerName  string `toml:"server_name"`
}

// ProxyHSTSConfig 代理 HSTS 配置
type ProxyHSTSConfig struct {
	Enabled             bool `toml:"enabled"`
	MaxAge              int  `toml:"max_age"`
	IncludeSubdomains   bool `toml:"include_subdomains"`
}

// ProxyPoolConfig 代理连接池配置
type ProxyPoolConfig struct {
	MaxConnections  int    `toml:"max_connections"`
	IdleTimeout    string `toml:"idle_timeout"`
	MaxLifetime    string `toml:"max_lifetime"`
}

// SQLWhitelist SQL 白名单配置
type SQLWhitelistConfig struct {
	Enabled        bool     `toml:"enabled"`
	AllowedTables  []string `toml:"allowed_tables"`
	ReadOnly       bool     `toml:"read_only"`
}

// ACLConfig 访问控制列表配置
type ACLConfig struct {
	Enabled   bool     `toml:"enabled"`
	AllowIPs  []string `toml:"allow_ips"`
	DenyIPs   []string `toml:"deny_ips"`
}

// NATConfig NAT 穿透配置
type NATConfig struct {
	Enabled           bool     `toml:"enabled"`
	STUNServers       []string `toml:"stun_servers"`
	KeepaliveInterval int      `toml:"keepalive_interval"`
}

// ProxyConnectionConfig 代理连接配置
type ProxyConnectionConfig struct {
	MaxConns     int    `toml:"max_conns"`
	IdleTimeout  string `toml:"idle_timeout"`
	DialTimeout  string `toml:"dial_timeout"`
	ReadTimeout  string `toml:"read_timeout"`
	WriteTimeout string `toml:"write_timeout"`
}

// RetryConfig 重试配置
type RetryConfig struct {
	MaxAttempts   int    `toml:"max_attempts"`
	RetryDelay    string `toml:"retry_delay"`
	RetryOnTimeout bool   `toml:"retry_on_timeout"`
}

// CircuitBreakerConfig 断路器配置
type CircuitBreakerConfig struct {
	Enabled         bool   `toml:"enabled"`
	FailureThreshold int   `toml:"failure_threshold"`
	SuccessThreshold int   `toml:"success_threshold"`
	Timeout         string `toml:"timeout"`
	HalfOpenRequests int  `toml:"half_open_requests"`
}

// ProxyRateLimitConfig 代理限流配置
type ProxyRateLimitConfig struct {
	Enabled             bool `toml:"enabled"`
	RequestsPerSecond   int  `toml:"requests_per_second"`
	Burst               int  `toml:"burst"`
}

// AccessLogConfig 访问日志配置
type AccessLogConfig struct {
	Enabled   bool   `toml:"enabled"`
	LogFile   string `toml:"log_file"`
	LogFormat string `toml:"log_format"`
}

// TransformConfig 转换配置
type TransformConfig struct {
	Type  string            `toml:"type"`
	Name  string            `toml:"name"`
	Value string            `toml:"value"`
	From  string            `toml:"from"`
	To    string            `toml:"to"`
	Headers map[string]string `toml:"headers"`
}

// SFTPConfig SFTP 配置
type SFTPConfig struct {
	EnableSFTP      bool     `toml:"enable_sftp"`
	MaxFileSize     string   `toml:"max_file_size"`
	AllowedCommands []string `toml:"allowed_commands"`
	Chroot          string   `toml:"chroot"`
}

// RDPConfig RDP 配置
type RDPConfig struct {
	EnableNLA     bool   `toml:"enable_nla"`
	EnableRDG     bool   `toml:"enable_rdg"`
	GatewayURL    string `toml:"gateway_url"`
}

// WebSocketConfig WebSocket 配置
type WebSocketConfig struct {
	Enabled            bool   `toml:"enabled"`
	AllowedOrigins     []string `toml:"allowed_origins"`
	PingInterval       string `toml:"ping_interval"`
	PongTimeout        string `toml:"pong_timeout"`
	MaxMessageSize     string `toml:"max_message_size"`
	ReadBufferSize     string `toml:"read_buffer_size"`
	WriteBufferSize    string `toml:"write_buffer_size"`
}

// ChainConfig 链式代理配置
type ChainConfig struct {
	Enabled            bool   `toml:"enabled"`
	Upstream           string `toml:"upstream"`
	UpstreamLocalPort  int    `toml:"upstream_local_port"`
}

// StaticFilesConfig 静态文件配置
type StaticFilesConfig struct {
	Enabled           bool     `toml:"enabled"`
	Root              string   `toml:"root"`
	Index             []string `toml:"index"`
	DirectoryListing  bool     `toml:"directory_listing"`
	CacheControl      string   `toml:"cache_control"`
	ETagEnabled       bool     `toml:"etag_enabled"`
	LastModifiedEnabled bool   `toml:"last_modified_enabled"`
	MaxUploadSize     string   `toml:"max_upload_size"`
	AllowedExtensions []string `toml:"allowed_extensions"`
}

// ============================================================================
// 配置加载和验证函数
// ============================================================================

// LoadServerConfig 加载服务端配置
func LoadServerConfig(path string) (*ServerConfig, error) {
	var cfg ServerConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// 设置默认值
	setServerDefaults(&cfg)

	return &cfg, nil
}

// setServerDefaults 设置服务端配置默认值
func setServerDefaults(cfg *ServerConfig) {
	if cfg.Server.BindAddr == "" {
		cfg.Server.BindAddr = "0.0.0.0"
	}
	if cfg.Server.BindPort == 0 {
		cfg.Server.BindPort = 7000
	}
	if cfg.Transport.TCPMuxKeepaliveInterval == 0 {
		cfg.Transport.TCPMuxKeepaliveInterval = 60
	}
	if cfg.Transport.MaxPoolCount == 0 {
		cfg.Transport.MaxPoolCount = 5
	}
	if cfg.Transport.MinPoolSize == 0 {
		cfg.Transport.MinPoolSize = 2
	}
	if cfg.Security.HeartbeatTimeout == 0 {
		cfg.Security.HeartbeatTimeout = 90
	}
	if cfg.Security.ConnectionTimeout == 0 {
		cfg.Security.ConnectionTimeout = 10
	}
	if cfg.Security.ReadTimeout == 0 {
		cfg.Security.ReadTimeout = 60
	}
	if cfg.Security.WriteTimeout == 0 {
		cfg.Security.WriteTimeout = 60
	}
	if cfg.Proxy.BindAddr == "" {
		cfg.Proxy.BindAddr = "0.0.0.0"
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "json"
	}
	if cfg.Session.StoreType == "" {
		cfg.Session.StoreType = "memory"
	}
	if cfg.Session.Expiry == 0 {
		cfg.Session.Expiry = time.Hour * 24
	}
	if cfg.Session.MaxSessions == 0 {
		cfg.Session.MaxSessions = 10000
	}
	if cfg.AdvancedCrypto.KDFType == "" {
		cfg.AdvancedCrypto.KDFType = "argon2id"
	}
	if cfg.AdvancedCrypto.KeyRotationInterval == 0 {
		cfg.AdvancedCrypto.KeyRotationInterval = time.Hour * 168 // 7天
	}
}

// LoadClientConfig 加载客户端配置
func LoadClientConfig(path string) (*ClientConfig, error) {
	var cfg ClientConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// 设置默认值
	setClientDefaults(&cfg)

	// 验证代理配置
	for i, pxy := range cfg.Proxies {
		if pxy.Name == "" {
			return nil, fmt.Errorf("proxy[%d]: name is required", i)
		}
		if pxy.Type == "" {
			return nil, fmt.Errorf("proxy[%s]: type is required", pxy.Name)
		}
		pxy.Type = strings.ToLower(pxy.Type)
		cfg.Proxies[i] = pxy
	}

	return &cfg, nil
}

// setClientDefaults 设置客户端配置默认值
func setClientDefaults(cfg *ClientConfig) {
	if cfg.Client.PoolCount == 0 {
		cfg.Client.PoolCount = 1
	}
	if cfg.Client.Protocol == "" {
		cfg.Client.Protocol = "tcp"
	}
	if cfg.Transport.TCPMuxKeepaliveInterval == 0 {
		cfg.Transport.TCPMuxKeepaliveInterval = 60
	}
	if cfg.Reconnect.Strategy == "" {
		cfg.Reconnect.Strategy = "exponential"
	}
	if cfg.Reconnect.Jitter == 0 {
		cfg.Reconnect.Jitter = 0.2
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "json"
	}
	if cfg.Statistics.Enabled {
		if cfg.Statistics.Interval == 0 {
			cfg.Statistics.Interval = 60
		}
		if cfg.Statistics.ExportFormat == "" {
			cfg.Statistics.ExportFormat = "json"
		}
	}
	if cfg.HealthCheck.Interval == 0 {
		cfg.HealthCheck.Interval = time.Second * 30
	}
	if cfg.Cache.Strategy == "" {
		cfg.Cache.Strategy = "lru"
	}
}

// Validate 验证服务端配置
func (c *ServerConfig) Validate() error {
	if c.Server.BindPort <= 0 || c.Server.BindPort > 65535 {
		return fmt.Errorf("invalid bind port: %d", c.Server.BindPort)
	}

	if c.TLS.Enabled {
		if c.TLS.CertFile == "" {
			return fmt.Errorf("TLS enabled but cert_file not specified")
		}
		if c.TLS.KeyFile == "" {
			return fmt.Errorf("TLS enabled but key_file not specified")
		}
		if _, err := os.Stat(c.TLS.CertFile); os.IsNotExist(err) {
			return fmt.Errorf("TLS cert file not found: %s", c.TLS.CertFile)
		}
		if _, err := os.Stat(c.TLS.KeyFile); os.IsNotExist(err) {
			return fmt.Errorf("TLS key file not found: %s", c.TLS.KeyFile)
		}
		if c.TLS.ClientAuth && c.TLS.CAFile == "" {
			return fmt.Errorf("client auth enabled but ca_file not specified")
		}
	}

	if c.Security.HeartbeatTimeout <= 0 {
		return fmt.Errorf("invalid heartbeat timeout: %d", c.Security.HeartbeatTimeout)
	}

	return nil
}

// Validate 验证客户端配置
func (c *ClientConfig) Validate() error {
	if c.Client.ServerAddr == "" {
		return fmt.Errorf("server_addr is required")
	}
	if c.Client.ServerPort <= 0 || c.Client.ServerPort > 65535 {
		return fmt.Errorf("invalid server port: %d", c.Client.ServerPort)
	}

	if len(c.Proxies) == 0 {
		return fmt.Errorf("at least one proxy configuration is required")
	}

	for _, pxy := range c.Proxies {
		if pxy.LocalPort <= 0 || pxy.LocalPort > 65535 {
			return fmt.Errorf("proxy[%s]: invalid local port: %d", pxy.Name, pxy.LocalPort)
		}

		if pxy.Type == "tcp" || pxy.Type == "udp" {
			if pxy.RemotePort <= 0 || pxy.RemotePort > 65535 {
				return fmt.Errorf("proxy[%s]: invalid remote port: %d", pxy.Name, pxy.RemotePort)
			}
		}
	}

	return nil
}
