package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The manual is read from the checkout; the guards below are about the
// configuration surface staying in step with it.
func readManual(t *testing.T) string {
	t.Helper()
	manual, err := os.ReadFile(filepath.Join("..", "..", "docs", "CONFIGURATION.md"))
	if err != nil {
		t.Skipf("the manual is not part of this checkout: %v", err)
	}
	return string(manual)
}

// readConfigSource returns the source text the key guard scans. Every non-test
// file of the package counts: a key that lives in a helper file — proxyopts.go,
// fragment.go, logoptions.go — is as undocumented as one in config.go when it is
// missing from the manual.
func readConfigSource(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the configuration source files: %v", err)
	}
	var source strings.Builder
	scanned := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		source.Write(body)
		source.WriteByte('\n')
		scanned++
	}
	if scanned == 0 {
		t.Fatal("no configuration source files were found next to this test")
	}
	return source.String()
}

// Every key a configuration file can carry is described in the manual.
//
// This lives next to the structs because it is a property of the configuration
// surface: a key that is parsed, validated and enforced but not documented is a
// feature an operator can only find by reading the source, and that is the gap
// this catches the next time a key is added.
func TestEveryKeyIsDocumented(t *testing.T) {
	source := readConfigSource(t)
	text := readManual(t)

	// Section names are documented as headings ("## `[server]`", a repeated
	// one as [[visitors]], and the root array as "## 顶层 `includes`"), keys
	// as `` `key` `` in a table row. Either form counts for the name it
	// carries; a mention in passing prose does not, because it would keep a
	// moved or renamed key looking documented long after its section stopped
	// describing it.
	documented := func(name string) bool {
		// The key has to be in the key column — the first cell of a table row.
		// One row may name two keys ("`plugin_user` / `plugin_password`"), and
		// either counts. Matching anywhere on the line instead let a key that is
		// only named in another key's description — health_check's interval_s and
		// timeout_s — count as documented, so its own entry could be deleted
		// without the guard noticing.
		for _, line := range strings.Split(text, "\n") {
			if !strings.HasPrefix(line, "|") {
				continue
			}
			cells := strings.Split(line, "|")
			if len(cells) > 1 && strings.Contains(cells[1], "`"+name+"`") {
				return true
			}
		}
		heading := regexp.MustCompile(`(?m)^#{1,6} \S.*\b` + regexp.QuoteMeta(name) + `\b`)
		return heading.MatchString(text)
	}

	seen := map[string]bool{}
	var undocumented []string
	for _, match := range regexp.MustCompile(`toml:"([a-z_0-9]+)"`).FindAllStringSubmatch(source, -1) {
		key := match[1]
		if seen[key] {
			continue
		}
		seen[key] = true
		if !documented(key) {
			undocumented = append(undocumented, key)
		}
	}
	if len(undocumented) > 0 {
		t.Fatalf("%d key(s) are not in docs/CONFIGURATION.md: %s",
			len(undocumented), strings.Join(undocumented, ", "))
	}
}

// Every section the manual describes is a section the configuration has, so a
// renamed or removed section cannot leave an entry describing nothing.
func TestEveryDocumentedSectionExists(t *testing.T) {
	source := readConfigSource(t)
	text := readManual(t)

	sections := regexp.MustCompile(`(?m)^##+ `+"`"+`\[{1,2}([a-z\.]+)\]{1,2}`+"`").FindAllStringSubmatch(text, -1)
	if len(sections) == 0 {
		t.Fatal("the manual describes no sections at all")
	}
	for _, match := range sections {
		name := match[1]
		// A nested section ([server.dashboard]) is the last element's field of
		// its parent, so the leaf is what the configuration carries.
		if index := strings.LastIndex(name, "."); index >= 0 {
			name = name[index+1:]
		}
		if !strings.Contains(source, `toml:"`+name+`"`) {
			t.Errorf("the manual documents [%s], which is not a section of the configuration", match[1])
		}
	}
}
