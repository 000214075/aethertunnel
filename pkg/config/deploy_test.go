package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Kubernetes manifests in this repository are a shipped deliverable, and the only
// thing that connects them to the program is the environment variable names. A manifest
// that sets a name this package does not read leaves the server without a credential,
// and the server then refuses to start — which is exactly what happened while the
// Deployment used `envFrom: secretRef` with short Secret keys ("auth-token").
//
// These tests read the manifests as text: that is enough for the names, and it keeps the
// repository free of a YAML dependency for a deployment that has three environment
// variables.
func readManifest(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join("..", "..", "deploy", "kubernetes", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// envNamesRead is the set of environment variables this package reads.
func envNamesRead() map[string]bool {
	return map[string]bool{
		EnvAuthToken:            true,
		EnvDashboardToken:       true,
		EnvEncryptionPassphrase: true,
	}
}

// Every AETHERTUNNEL_* name a manifest mentions has to be one the program reads: a
// misspelt or renamed variable would be silently ignored.
func TestManifestsOnlyNameEnvironmentThisPackageReads(t *testing.T) {
	known := envNamesRead()
	pattern := regexp.MustCompile(`AETHERTUNNEL_[A-Z_]+`)

	matches, err := filepath.Glob(filepath.Join("..", "..", "deploy", "kubernetes", "*.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no manifests found")
	}
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, name := range pattern.FindAllString(string(raw), -1) {
			if !known[name] {
				t.Errorf("%s names %s, which this package does not read", filepath.Base(path), name)
			}
		}
	}
}

// withoutComments drops whole-line comments, so a manifest cannot satisfy a structural
// check in prose — or fail one by explaining the trap it avoids.
func withoutComments(manifest string) string {
	var kept []string
	for _, line := range strings.Split(manifest, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// The Deployment has to supply the credential the server insists on. The Secret's keys
// are not the variable names, so the mapping has to be explicit: `envFrom: secretRef`
// copies keys verbatim and would never produce these names.
func TestTheDeploymentSuppliesTheAuthTokenTheServerRequires(t *testing.T) {
	deployment := withoutComments(readManifest(t, "deployment.yaml"))
	secret := readManifest(t, "secret.example.yaml")

	provided := regexp.MustCompile(`(?m)^\s*- name:\s*` + regexp.QuoteMeta(EnvAuthToken) + `\s*$`)
	if !provided.MatchString(deployment) {
		t.Errorf("deployment.yaml does not set %s, so the server would refuse to start", EnvAuthToken)
	}
	if strings.Contains(deployment, "envFrom:") {
		t.Error("deployment.yaml uses envFrom: the Secret's short keys would become variables nothing reads; " +
			"map them with env/valueFrom/secretKeyRef instead")
	}
	// The value must come from the Secret rather than sitting in the manifest.
	if regexp.MustCompile(`(?m)name:\s*` + regexp.QuoteMeta(EnvAuthToken) + `\s*\n\s*value:`).MatchString(deployment) {
		t.Errorf("%s is set from a literal in the manifest instead of the Secret", EnvAuthToken)
	}

	// The keys the Deployment reads must be the keys the Secret documents.
	for _, key := range []string{"auth-token:", "dashboard-token:"} {
		if !strings.Contains(secret, key) {
			t.Errorf("secret.example.yaml does not define %s, which deployment.yaml reads", strings.TrimSuffix(key, ":"))
		}
	}
}

// The ConfigMap holds the server configuration, so it has to be one this build accepts.
// It carries no credential by design: the Deployment supplies those.
func TestTheConfigMapHoldsAConfigThatValidates(t *testing.T) {
	configMap := readManifest(t, "configmap.yaml")

	block := strings.Index(configMap, "server.toml: |")
	if block < 0 {
		t.Fatal("configmap.yaml has no server.toml key")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "server.toml")

	var lines []string
	for _, line := range strings.Split(configMap[block:], "\n")[1:] {
		if strings.TrimSpace(line) == "" {
			lines = append(lines, "")
			continue
		}
		if !strings.HasPrefix(line, "    ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(line, "    "))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write the extracted config: %v", err)
	}

	// The Deployment supplies these, so the check runs with them set.
	t.Setenv(EnvAuthToken, "0123456789abcdef0123456789abcdef")
	t.Setenv(EnvDashboardToken, "0123456789abcdef0123456789abcdef")

	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("the ConfigMap's server.toml is not valid: %v", err)
	}
	if cfg.Server.AuthToken == "" {
		t.Error("the loaded configuration has no auth token, so the deployment would not authenticate anyone")
	}
}
