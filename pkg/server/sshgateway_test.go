package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	flynet "github.com/aethertunnel/aethertunnel/pkg/net"
)

// sshGatewayConfig returns a validated server configuration with the ssh tunnel
// gateway enabled on its own port.
func sshGatewayConfig(t *testing.T, keyPath string) (*config.Config, int) {
	t.Helper()
	cfg := testConfig(t, false)
	port := freePort(t)
	cfg.Server.SSHTunnelGateway = &config.SSHTunnelGatewayConfig{
		BindAddr: "127.0.0.1",
		BindPort: port,
		KeyFile:  keyPath,
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("gateway test config invalid: %v", err)
	}
	return cfg, port
}

func newTestSSHSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ssh test key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatalf("ssh signer: %v", err)
	}
	return signer
}

func dialSSHGateway(t *testing.T, port int) *ssh.Client {
	t.Helper()
	waitForTCPPort(t, port)
	client, err := ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &ssh.ClientConfig{
		User:            "v0",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(newTestSSHSigner(t))},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial ssh gateway: %v", err)
	}
	return client
}

// gatewayPortReady reports whether something accepts a TCP connection on the
// port before the deadline. startServer returns as soon as the server's control
// listener is up, and the gateway's own listener is bound later in Run, so a test
// that dials the gateway straight away is refused on a loaded machine.
func gatewayPortReady(port int, within time.Duration) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(within)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForTCPPort is gatewayPortReady for a dial the test expects to succeed.
func waitForTCPPort(t *testing.T, port int) {
	t.Helper()
	if !gatewayPortReady(port, 5*time.Second) {
		t.Fatalf("nothing is listening on 127.0.0.1:%d", port)
	}
}

// readUntil reads from r until substr appears or the stream ends, and fails the
// test on a timeout. The exec channel stays open by design, so waiting for the
// process to finish is not an option.
func readSSHUntil(t *testing.T, r io.Reader, substr string, timeout time.Duration) string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 256)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
				if strings.Contains(b.String(), substr) {
					done <- b.String()
					return
				}
			}
			if err != nil {
				done <- b.String()
				return
			}
		}
	}()
	select {
	case got := <-done:
		if !strings.Contains(got, substr) {
			t.Fatalf("ssh output did not contain %q; got %q", substr, got)
		}
		return got
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %q in ssh output", substr)
		return ""
	}
}

// serveForwardedToEcho services the forwarded-tcpip channels the gateway opens:
// every one is connected to the echo service, which is exactly what OpenSSH does
// with the -R target.
func serveForwardedToEcho(listener net.Listener, echoAddr string) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			local, err := net.Dial("tcp", echoAddr)
			if err != nil {
				_ = conn.Close()
				return
			}
			// No idle timeout: the ssh package's channel conn answers
			// SetReadDeadline with "deadline not supported", and Pipe sets one on
			// each source. The gateway's own sshChannelConn hides them the same
			// way, on its side.
			flynet.Pipe(conn, local, 0)
		}()
	}
}

// sshGatewayConfigWith is sshGatewayConfig with a chance to change the gateway
// section before the configuration is validated again, so a test can add
// credentials or an authorized_keys_file.
func sshGatewayConfigWith(t *testing.T, keyPath string, mutate func(*config.SSHTunnelGatewayConfig)) (*config.Config, int) {
	t.Helper()
	cfg, port := sshGatewayConfig(t, keyPath)
	mutate(cfg.Server.SSHTunnelGateway)
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("gateway test config invalid: %v", err)
	}
	return cfg, port
}

// dialSSHGatewayAs dials the gateway with an explicit client configuration and
// returns the error rather than failing the test, so a caller can assert that
// authentication was refused.
func dialSSHGatewayAs(port int, user string, auth []ssh.AuthMethod) (*ssh.Client, error) {
	// Same readiness grace as dialSSHGateway: a refusal here has to be the SSH
	// handshake's, not a listener that is not up yet.
	gatewayPortReady(port, time.Second)
	return ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port of %q: %v", addr, err)
	}
	return number
}

// TestSSHGatewayForwardsTrafficEndToEnd drives the gateway with a real
// golang.org/x/crypto/ssh client: authenticate, run the exec command, accept the
// forwarded-tcpip channel it opens, and move bytes so a visitor on the published
// port reaches the echo service.
func TestSSHGatewayForwardsTrafficEndToEnd(t *testing.T) {
	echoAddr := startEcho(t)
	echoPort := portOf(t, echoAddr)
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")

	cfg, gwPort := sshGatewayConfig(t, keyPath)
	rs := startServerLoggingTo(t, cfg, os.Stderr)
	_ = rs

	client := dialSSHGateway(t, gwPort)
	defer client.Close()

	forwarded, err := client.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("request a remote forwarding: %v", err)
	}
	defer forwarded.Close()
	go serveForwardedToEcho(forwarded, echoAddr)

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	publicPort := freePort(t)
	command := fmt.Sprintf("tcp --proxy_name web --remote_port %d --local_ip 127.0.0.1 --local_port %d --token %s",
		publicPort, echoPort, testToken)
	if err := session.Start(command); err != nil {
		t.Fatalf("exec %q: %v", command, err)
	}

	banner := readSSHUntil(t, stdout, "RemoteAddress:", 5*time.Second)
	for _, want := range []string{"User: v0", "ProxyName: web", "Type: tcp"} {
		if !strings.Contains(banner, want) {
			t.Fatalf("banner %q does not contain %q", banner, want)
		}
	}

	visitor, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort)), 5*time.Second)
	if err != nil {
		t.Fatalf("visitor dial: %v", err)
	}
	defer visitor.Close()

	message := []byte("hello through the ssh tunnel gateway")
	if _, err := visitor.Write(message); err != nil {
		t.Fatalf("visitor write: %v", err)
	}
	got := make([]byte, len(message))
	if _, err := io.ReadFull(visitor, got); err != nil {
		t.Fatalf("visitor read: %v", err)
	}
	if string(got) != string(message) {
		t.Fatalf("echo mismatch: sent %q, got %q", message, got)
	}
}

// TestSSHGatewayTokenCheck covers the three token outcomes. SSH authentication
// comes first, and the token second, exactly as frp orders them.
func TestSSHGatewayTokenCheck(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg, gwPort := sshGatewayConfig(t, keyPath)
	startServer(t, cfg)

	cases := []struct {
		name    string
		command string
		want    string
	}{
		{"missing", "tcp --proxy_name web --remote_port %d", "authentication failed"},
		{"wrong", "tcp --proxy_name web --remote_port %d --token not-the-token", "authentication failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := dialSSHGateway(t, gwPort)
			defer client.Close()
			if _, err := client.Listen("tcp", "127.0.0.1:0"); err != nil {
				t.Fatalf("request a remote forwarding: %v", err)
			}

			session, err := client.NewSession()
			if err != nil {
				t.Fatalf("new session: %v", err)
			}
			stdout, err := session.StdoutPipe()
			if err != nil {
				t.Fatalf("stdout pipe: %v", err)
			}
			command := fmt.Sprintf(tc.command, freePort(t))
			if err := session.Start(command); err != nil {
				t.Fatalf("exec: %v", err)
			}
			readSSHUntil(t, stdout, tc.want, 5*time.Second)
		})
	}

	t.Run("right", func(t *testing.T) {
		client := dialSSHGateway(t, gwPort)
		defer client.Close()
		if _, err := client.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatalf("request a remote forwarding: %v", err)
		}
		session, err := client.NewSession()
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		stdout, err := session.StdoutPipe()
		if err != nil {
			t.Fatalf("stdout pipe: %v", err)
		}
		command := fmt.Sprintf("tcp --proxy_name web --remote_port %d --token %s", freePort(t), testToken)
		if err := session.Start(command); err != nil {
			t.Fatalf("exec: %v", err)
		}
		readSSHUntil(t, stdout, "ProxyName: web", 5*time.Second)
	})
}

// TestSSHGatewayRefusesUnknownFlag checks that a bad command line is answered
// with the accepted flags and leaves the session usable: the SSH connection and
// its remote forwarding stay up, so another command can be sent.
func TestSSHGatewayRefusesUnknownFlag(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg, gwPort := sshGatewayConfig(t, keyPath)
	startServer(t, cfg)

	client := dialSSHGateway(t, gwPort)
	defer client.Close()
	if _, err := client.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatalf("request a remote forwarding: %v", err)
	}

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := session.Start("tcp --proxy_name web --remote_port 1 --token " + testToken + " --bogus 1"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	readSSHUntil(t, stdout, `unknown flag "--bogus"`, 5*time.Second)

	// The session is still usable: a second command registers a proxy on the
	// same SSH connection.
	second, err := client.NewSession()
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	secondOut, err := second.StdoutPipe()
	if err != nil {
		t.Fatalf("second stdout pipe: %v", err)
	}
	command := fmt.Sprintf("tcp --proxy_name second --remote_port %d --token %s", freePort(t), testToken)
	if err := second.Start(command); err != nil {
		t.Fatalf("second exec: %v", err)
	}
	readSSHUntil(t, secondOut, "ProxyName: second", 5*time.Second)
}

// TestSSHGatewayHelp checks that --help prints the accepted flags and closes the
// session cleanly.
func TestSSHGatewayHelp(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg, gwPort := sshGatewayConfig(t, keyPath)
	startServer(t, cfg)

	client := dialSSHGateway(t, gwPort)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := session.Start("--help"); err != nil {
		t.Fatalf("exec --help: %v", err)
	}
	// Wait for the last line of the flag list rather than its first: readSSHUntil
	// returns as soon as it has the substring, and "Usage:" arrives in the same
	// read as the beginning of the list, so anchoring there checked a partial
	// buffer.
	usage := readSSHUntil(t, stdout, "--help", 5*time.Second)
	for _, flag := range []string{"--proxy_name", "--remote_port", "--custom_domains", "--secret_key", "--token"} {
		if !strings.Contains(usage, flag) {
			t.Fatalf("usage does not list %s: %q", flag, usage)
		}
	}

	// The session was closed cleanly, so the connection is gone.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := client.NewSession(); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the gateway did not close the session after --help")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSSHGatewayMaxProxies checks that one SSH session cannot publish more
// proxies than max_proxies allows.
func TestSSHGatewayMaxProxies(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg := testConfig(t, false)
	port := freePort(t)
	cfg.Server.SSHTunnelGateway = &config.SSHTunnelGatewayConfig{
		BindAddr:   "127.0.0.1",
		BindPort:   port,
		KeyFile:    keyPath,
		MaxProxies: 1,
	}
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	startServer(t, cfg)

	client := dialSSHGateway(t, port)
	defer client.Close()
	if _, err := client.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatalf("request a remote forwarding: %v", err)
	}

	first, err := client.NewSession()
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	firstOut, err := first.StdoutPipe()
	if err != nil {
		t.Fatalf("first stdout: %v", err)
	}
	if err := first.Start(fmt.Sprintf("tcp --proxy_name one --remote_port %d --token %s", freePort(t), testToken)); err != nil {
		t.Fatalf("first exec: %v", err)
	}
	readSSHUntil(t, firstOut, "ProxyName: one", 5*time.Second)

	second, err := client.NewSession()
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	secondOut, err := second.StdoutPipe()
	if err != nil {
		t.Fatalf("second stdout: %v", err)
	}
	if err := second.Start(fmt.Sprintf("tcp --proxy_name two --remote_port %d --token %s", freePort(t), testToken)); err != nil {
		t.Fatalf("second exec: %v", err)
	}
	readSSHUntil(t, secondOut, "maximum of 1 proxies", 5*time.Second)
}

// TestSSHGatewayGeneratesHostKey checks that a missing host key file is created
// with 0600 and that a later start reads the same key back.
func TestSSHGatewayGeneratesHostKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "gateway_host_key")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the key file should not exist yet: %v", err)
	}

	first, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the generated host key was not written: %v", err)
	}
	// Windows has no POSIX permission bits: Stat reports 0666 for every file, so the
	// mode is only meaningful elsewhere.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("host key mode is %o, want 600", perm)
		}
	}

	second, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("reload host key: %v", err)
	}
	if ssh.FingerprintSHA256(first.PublicKey()) != ssh.FingerprintSHA256(second.PublicKey()) {
		t.Fatal("the host key changed between reads; a pinned client would break")
	}
}

// TestSSHGatewayForwardsThroughRealSSH uses the installed OpenSSH client, which
// is the client an operator actually uses. It is skipped when ssh or ssh-keygen
// is not on PATH.
func TestSSHGatewayForwardsThroughRealSSH(t *testing.T) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh is not installed")
	}
	keygenPath, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen is not installed")
	}

	echoAddr := startEcho(t)
	echoPort := portOf(t, echoAddr)
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")

	cfg, gwPort := sshGatewayConfig(t, keyPath)
	startServer(t, cfg)

	clientKey := filepath.Join(t.TempDir(), "id_ed25519")
	if out, err := exec.Command(keygenPath, "-q", "-t", "ed25519", "-N", "", "-f", clientKey).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v (%s)", err, out)
	}

	publicPort := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, sshPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "LogLevel=ERROR",
		"-T",
		"-i", clientKey,
		"-p", strconv.Itoa(gwPort),
		"-R", ":2222:127.0.0.1:"+strconv.Itoa(echoPort),
		"v0@127.0.0.1",
		"tcp", "--proxy_name", "sshdemo", "--remote_port", strconv.Itoa(publicPort), "--token", testToken,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ssh: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	readSSHUntil(t, stdout, "ProxyName: sshdemo", 15*time.Second)

	visitor, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort)), 5*time.Second)
	if err != nil {
		t.Fatalf("visitor dial: %v", err)
	}
	defer visitor.Close()

	message := []byte("hello from an OpenSSH client")
	if _, err := visitor.Write(message); err != nil {
		t.Fatalf("visitor write: %v", err)
	}
	got := make([]byte, len(message))
	if _, err := io.ReadFull(visitor, got); err != nil {
		t.Fatalf("visitor read: %v", err)
	}
	if string(got) != string(message) {
		t.Fatalf("echo mismatch: sent %q, got %q", message, got)
	}
}

// TestSSHGatewayAcceptsAnyKeyWhenNothingIsConfigured pins the default an
// operator gets from an empty gateway section: with no authorized_keys_file and
// no password, every key that authenticates opens a session. The warning the
// configuration emits for that combination says the same thing.
func TestSSHGatewayAcceptsAnyKeyWhenNothingIsConfigured(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg, gwPort := sshGatewayConfig(t, keyPath)
	if warnings := strings.Join(cfg.Warnings, "\n"); !strings.Contains(warnings, "any SSH client") {
		t.Fatalf("no warning about the open gateway: %v", cfg.Warnings)
	}
	startServer(t, cfg)

	client, err := dialSSHGatewayAs(gwPort, "v0", []ssh.AuthMethod{ssh.PublicKeys(newTestSSHSigner(t))})
	if err != nil {
		t.Fatalf("a key should be admitted when nothing is configured: %v", err)
	}
	defer client.Close()
}

// TestSSHGatewayAuthorizedKeysFileAdmitsOnlyListedKeys checks that a file turns
// the gateway from "any key" into "these keys": the signer whose public key is
// in the file is admitted, another one is refused.
func TestSSHGatewayAuthorizedKeysFileAdmitsOnlyListedKeys(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	listed := newTestSSHSigner(t)
	authorized := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(authorized, ssh.MarshalAuthorizedKey(listed.PublicKey()), 0o600); err != nil {
		t.Fatalf("write authorized_keys: %v", err)
	}

	cfg, gwPort := sshGatewayConfigWith(t, keyPath, func(g *config.SSHTunnelGatewayConfig) {
		g.AuthorizedKeysFile = authorized
	})
	startServer(t, cfg)

	client, err := dialSSHGatewayAs(gwPort, "v0", []ssh.AuthMethod{ssh.PublicKeys(listed)})
	if err != nil {
		t.Fatalf("the listed key should be admitted: %v", err)
	}
	defer client.Close()

	stranger, err := dialSSHGatewayAs(gwPort, "v0", []ssh.AuthMethod{ssh.PublicKeys(newTestSSHSigner(t))})
	if err == nil {
		stranger.Close()
		t.Fatal("a key that is not in authorized_keys_file was admitted")
	}
}

// TestSSHGatewayPasswordExcludesKeysWhenNoKeyFileIsSet covers the combination
// that used to be wrong: a shared password with no authorized_keys_file. The
// password authenticates, and any key is refused, because a gateway that asks
// for a password is not also open to every key that turns up.
func TestSSHGatewayPasswordExcludesKeysWhenNoKeyFileIsSet(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg, gwPort := sshGatewayConfigWith(t, keyPath, func(g *config.SSHTunnelGatewayConfig) {
		g.User = "ops"
		g.Password = "s3cret"
	})
	startServer(t, cfg)

	client, err := dialSSHGatewayAs(gwPort, "ops", []ssh.AuthMethod{ssh.Password("s3cret")})
	if err != nil {
		t.Fatalf("the configured password should be admitted: %v", err)
	}
	defer client.Close()

	wrong, err := dialSSHGatewayAs(gwPort, "ops", []ssh.AuthMethod{ssh.Password("guess")})
	if err == nil {
		wrong.Close()
		t.Fatal("the wrong password was admitted")
	}

	key, err := dialSSHGatewayAs(gwPort, "ops", []ssh.AuthMethod{ssh.PublicKeys(newTestSSHSigner(t))})
	if err == nil {
		key.Close()
		t.Fatal("a key was admitted although the gateway authenticates with a password and lists no key")
	}
}

// TestSSHGatewayAcceptsBothWhenBothAreConfigured checks the third combination:
// an authorized_keys_file and a password together. Either credential opens a
// session, and a key that is not in the file is still refused.
func TestSSHGatewayAcceptsBothWhenBothAreConfigured(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	listed := newTestSSHSigner(t)
	authorized := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(authorized, ssh.MarshalAuthorizedKey(listed.PublicKey()), 0o600); err != nil {
		t.Fatalf("write authorized_keys: %v", err)
	}

	cfg, gwPort := sshGatewayConfigWith(t, keyPath, func(g *config.SSHTunnelGatewayConfig) {
		g.AuthorizedKeysFile = authorized
		g.User = "ops"
		g.Password = "s3cret"
	})
	startServer(t, cfg)

	byKey, err := dialSSHGatewayAs(gwPort, "v0", []ssh.AuthMethod{ssh.PublicKeys(listed)})
	if err != nil {
		t.Fatalf("the listed key should be admitted: %v", err)
	}
	byKey.Close()

	byPassword, err := dialSSHGatewayAs(gwPort, "ops", []ssh.AuthMethod{ssh.Password("s3cret")})
	if err != nil {
		t.Fatalf("the password should be admitted alongside the key: %v", err)
	}
	byPassword.Close()

	unlisted, err := dialSSHGatewayAs(gwPort, "v0", []ssh.AuthMethod{ssh.PublicKeys(newTestSSHSigner(t))})
	if err == nil {
		unlisted.Close()
		t.Fatal("a key that is not in authorized_keys_file was admitted")
	}
}

// tcpmux and socks5 each need a key this command line does not carry
// (--multiplexer, --allow_targets), so their registrations are refused. The help
// no longer lists them; this pins the refusal so nothing can advertise a type the
// gateway can never publish.
func TestSSHGatewayRefusesTheTypesItsCommandLineCannotCarry(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")
	cfg, gwPort := sshGatewayConfig(t, keyPath)
	startServer(t, cfg)

	client := dialSSHGateway(t, gwPort)
	defer client.Close()
	if _, err := client.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatalf("request a remote forwarding: %v", err)
	}

	cases := []struct {
		command string
		want    string
	}{
		{"tcpmux --proxy_name web --custom_domain a.example --token " + testToken, "multiplexer"},
		{"socks5 --proxy_name web --remote_port " + strconv.Itoa(freePort(t)) + " --token " + testToken, "allow_targets"},
	}
	for _, tc := range cases {
		session, err := client.NewSession()
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		out, err := session.StdoutPipe()
		if err != nil {
			t.Fatalf("stdout pipe: %v", err)
		}
		if err := session.Start(tc.command); err != nil {
			t.Fatalf("exec %q: %v", tc.command, err)
		}
		readSSHUntil(t, out, tc.want, 5*time.Second)
		_ = session.Close()
	}
}

// The startup line is what an operator reads to know how the gateway admits a
// client. A user without a password registers no method at all, so it must not
// be described as "any ssh key" — which is what the line used to say.
func TestTheSSHGatewayStartupLineMatchesWhatItAdmits(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.SSHTunnelGatewayConfig
		keys int
		want string
	}{
		{"nothing configured admits any key", config.SSHTunnelGatewayConfig{}, 0, "any ssh key"},
		{"a key file names its keys", config.SSHTunnelGatewayConfig{AuthorizedKeysFile: "keys"}, 2, "2 authorized key(s)"},
		{"a password alone excludes keys", config.SSHTunnelGatewayConfig{User: "ops", Password: "s3cret"}, 0, "shared password"},
		{"both are offered", config.SSHTunnelGatewayConfig{AuthorizedKeysFile: "keys", User: "ops", Password: "s3cret"}, 1, "1 authorized key(s), shared password"},
		{"a user without a password admits nobody", config.SSHTunnelGatewayConfig{User: "ops"}, 0, "none"},
	}
	for _, tc := range cases {
		authorized := make(map[string]bool, tc.keys)
		for i := 0; i < tc.keys; i++ {
			authorized[strconv.Itoa(i)] = true
		}
		g := &sshTunnelGateway{cfg: &tc.cfg, authorized: authorized}
		if got := g.authDescription(); got != tc.want {
			t.Errorf("%s: authDescription() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
