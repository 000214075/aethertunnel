// Command smoketest provides the small local services the end-to-end smoke test
// needs, so the test does not depend on Python or any other runtime being
// installed.
//
// It starts a TCP echo, a UDP echo and a minimal HTTP server on loopback, prints
// their addresses as one JSON object on stdout, and then serves until it is
// killed.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type endpoints struct {
	TCP  string `json:"tcp"`
	UDP  string `json:"udp"`
	HTTP string `json:"http"`
}

func main() {
	host := flag.String("host", "127.0.0.1", "address family to bind")
	certDir := flag.String("cert-dir", "", "write a self-signed certificate for -cert-hosts into this directory and exit")
	certHosts := flag.String("cert-hosts", "127.0.0.1,localhost", "comma separated names the certificate is valid for")
	flag.Parse()

	logger := log.New(os.Stderr, "smoketest: ", log.LstdFlags)

	if *certDir != "" {
		if err := writeCertificate(*certDir, splitHosts(*certHosts), logger); err != nil {
			logger.Fatalf("certificate: %v", err)
		}
		return
	}

	result := endpoints{}

	tcpListener, err := net.Listen("tcp", *host+":0")
	if err != nil {
		logger.Fatalf("tcp echo: %v", err)
	}
	result.TCP = tcpListener.Addr().String()
	go serveTCPEcho(tcpListener, logger)

	udpSocket, err := net.ListenPacket("udp", *host+":0")
	if err != nil {
		logger.Fatalf("udp echo: %v", err)
	}
	result.UDP = udpSocket.LocalAddr().String()
	go serveUDPEcho(udpSocket, logger)

	httpListener, err := net.Listen("tcp", *host+":0")
	if err != nil {
		logger.Fatalf("http: %v", err)
	}
	result.HTTP = httpListener.Addr().String()
	go serveHTTP(httpListener, logger)

	encoded, err := json.Marshal(result)
	if err != nil {
		logger.Fatalf("encode: %v", err)
	}
	fmt.Println(string(encoded))

	select {}
}

func serveTCPEcho(listener net.Listener, logger *log.Logger) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_, _ = io.Copy(conn, conn)
		}()
	}
}

func serveUDPEcho(socket net.PacketConn, logger *log.Logger) {
	buf := make([]byte, 4096)
	for {
		n, addr, err := socket.ReadFrom(buf)
		if err != nil {
			return
		}
		if _, err := socket.WriteTo(buf[:n], addr); err != nil {
			return
		}
	}
}

func serveHTTP(listener net.Listener, logger *log.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "smoketest-http host=%s path=%s", r.Host, r.URL.Path)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	_ = server.Serve(listener)
}

func splitHosts(text string) []string {
	parts := strings.Split(text, ",")
	hosts := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			hosts = append(hosts, trimmed)
		}
	}
	return hosts
}

// writeCertificate creates a self-signed certificate so the smoke test can
// exercise the TLS transport and the https virtual host without depending on a
// certificate authority.
func writeCertificate(dir string, hosts []string, logger *log.Logger) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0]},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
			continue
		}
		template.DNSNames = append(template.DNSNames, host)
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return err
	}

	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}

	logger.Printf("wrote %s and %s for %v", certPath, keyPath, hosts)
	return nil
}
