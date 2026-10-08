package config

import (
	"os"
	"path"
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

// readRepoFile reads a file by its path relative to the module root.
func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()

	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// The image's default command names a configuration file, and that file exists in a
// container only because the Deployment mounts the ConfigMap at a directory. Nothing
// else connects the two: if they drift apart the pod comes up and exits before it
// listens, and a plain `docker run` fails on a path the image does not contain.
func TestTheImageExpectsTheConfigTheDeploymentMounts(t *testing.T) {
	dockerfile := withoutComments(readRepoFile(t, "Dockerfile"))
	deployment := withoutComments(readManifest(t, "deployment.yaml"))

	cmd := regexp.MustCompile(`CMD\s*\[\s*"--config"\s*,\s*"([^"]+)"\s*\]`).FindStringSubmatch(dockerfile)
	if cmd == nil {
		t.Fatal("the Dockerfile has no CMD [\"--config\", \"...\"] to check")
	}
	configPath := cmd[1]

	mount := regexp.MustCompile(`(?m)^\s*- name: config\s*\n\s*mountPath:\s*(\S+)\s*$`).FindStringSubmatch(deployment)
	if mount == nil {
		t.Fatal("the Deployment does not mount a volume named config")
	}

	// The ConfigMap's key is the file name inside that directory.
	block := regexp.MustCompile(`(?m)^\s\s([A-Za-z0-9._-]+):\s*\|`).FindStringSubmatch(readManifest(t, "configmap.yaml"))
	if block == nil {
		t.Fatal("the ConfigMap has no block scalar to name")
	}
	// Container paths are POSIX on every platform, including the one running the
	// test: filepath.Join would write \etc\... on Windows and compare unequal to
	// what the image really starts with.
	want := path.Join(mount[1], block[1])
	if configPath != want {
		t.Errorf("the image starts with --config %s, but the Deployment provides %s", configPath, want)
	}

	// The ports the image advertises are the ones the Deployment and the Service route
	// to: an image that exposes a port nothing forwards is a port nobody can reach.
	exported := regexp.MustCompile(`(?m)^EXPOSE\s+(.+)$`).FindStringSubmatch(dockerfile)
	if exported == nil {
		t.Fatal("the Dockerfile does not EXPOSE anything")
	}
	service := withoutComments(readManifest(t, "service.yaml"))
	for _, port := range []string{"7001", "7003", "7500"} {
		if !strings.Contains(exported[1], port) {
			t.Errorf("the Dockerfile does not EXPOSE %s (it exposes %q)", port, exported[1])
		}
		if !strings.Contains(deployment, "containerPort: "+port) {
			t.Errorf("the Deployment does not route container port %s", port)
		}
		if !strings.Contains(service, "port: "+port) {
			t.Errorf("the Service does not publish port %s", port)
		}
	}
}

// A Service merges its ports by number, so two entries that share one are not both
// applied: `kubectl apply` keeps the first and reports success, which is how a control
// port and a UDP DHT node on 7001 lost one another. Every port here needs its own
// number, and the Deployment and the ConfigMap have to agree with the Service.
func TestTheServiceGivesEveryPortItsOwnNumber(t *testing.T) {
	service := withoutComments(readManifest(t, "service.yaml"))

	numbers := regexp.MustCompile(`(?m)^\s*port:\s*(\d+)\s*$`).FindAllStringSubmatch(service, -1)
	if len(numbers) < 2 {
		t.Fatalf("the Service publishes %d ports; this check needs at least two", len(numbers))
	}
	seen := map[string]bool{}
	for _, match := range numbers {
		if seen[match[1]] {
			t.Errorf("service.yaml publishes port %s twice: a Service merges its ports by number, "+
				"so kubectl apply would silently apply only one of them", match[1])
		}
		seen[match[1]] = true
	}

	// The DHT is the port most likely to be left behind by a rename, because it is the
	// only one the ConfigMap names as well.
	configMap := readManifest(t, "configmap.yaml")
	listen := regexp.MustCompile(`(?m)^\s*listen_addr\s*=\s*"0\.0\.0\.0:(\d+)"`).FindAllStringSubmatch(configMap, -1)
	if len(listen) == 0 {
		t.Fatal("the ConfigMap's config binds no wildcard address")
	}
	for _, match := range listen {
		if !seen[match[1]] {
			t.Errorf("the ConfigMap binds UDP port %s, which the Service does not publish", match[1])
		}
	}
}

// The [vpn] recipe in the Deployment has to be written as edits to keys that are
// already there, and it has to name the uid the tunnel cannot run as.
//
// The version of it that was a block to uncomment duplicated this container's
// securityContext and volumeMounts, and put a `volumes` key inside the container,
// where Kubernetes has no such field. A parser that keeps the last copy would have
// dropped readOnlyRootFilesystem, allowPrivilegeEscalation: false and the state
// mount, and the pod would still have failed to open the tun device, because a
// process whose uid is not 0 does not receive a capability the container adds.
// Measured against docker 20.10.24, 28.4.0 and 29.2.1: `--user 65532:65532
// --cap-add NET_ADMIN --device /dev/net/tun` answered "TUNSETIFF ... operation not
// permitted" while the same run as uid 0 opened the device.
func TestTheVPNRecipeIsEditsRatherThanABlockThatDuplicatesKeys(t *testing.T) {
	deployment := readManifest(t, "deployment.yaml")

	for _, key := range []string{"securityContext", "volumeMounts", "volumes"} {
		block := regexp.MustCompile(`(?m)^\s*#\s*` + key + `:\s*$`)
		if block.MatchString(deployment) {
			t.Errorf("the Deployment offers a commented %q block: uncommented it is a "+
				"duplicate key, and the copy that wins is the one that loses the other's "+
				"settings. Write the recipe as an edit to the key that is already there.", key)
		}
	}

	// The capability is not enough on its own, so the recipe has to say so. This is
	// checked on commented lines only: the pod's own runAsUser is 65532, which would
	// otherwise satisfy the check by naming the very setting the recipe has to lift.
	for _, want := range []string{"runAsUser", "uid"} {
		mentioned := regexp.MustCompile(`(?m)^\s*#.*` + want).MatchString(deployment)
		if !mentioned {
			t.Errorf("the [vpn] recipe does not mention %s: a process whose uid is not 0 "+
				"never receives the capability it asks for, so the recipe has to say the "+
				"pod stops being non-root", want)
		}
	}
}

// Everything the server writes has to land in the volume the Deployment mounts, or it
// writes into the container's own filesystem and loses the audit log, the ledger and the
// signing keys when the pod is replaced.
func TestTheConfigOnlyWritesStateWhereTheDeploymentMountsIt(t *testing.T) {
	deployment := withoutComments(readManifest(t, "deployment.yaml"))
	mount := regexp.MustCompile(`(?m)^\s*- name: state\s*\n\s*mountPath:\s*(\S+)\s*$`).FindStringSubmatch(deployment)
	if mount == nil {
		t.Fatal("the Deployment does not mount a volume named state")
	}

	configMap := readManifest(t, "configmap.yaml")
	keys := regexp.MustCompile(`(?m)^\s*(path|signing_key_file|key_file)\s*=\s*"([^"]+)"`).FindAllStringSubmatch(configMap, -1)
	if len(keys) == 0 {
		t.Fatal("the ConfigMap's config sets no file path at all")
	}
	for _, match := range keys {
		if !strings.HasPrefix(match[2], mount[1]+"/") {
			t.Errorf("%s = %q is outside the state volume at %s", match[1], match[2], mount[1])
		}
	}
}
