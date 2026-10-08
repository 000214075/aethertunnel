package config

import "fmt"

// LoadFragment parses a configuration that carries only [[proxies]] and
// [[visitors]] — the shape a [store] file and the client's dynamic-entry API
// use — and returns those entries with exactly the defaults and the unknown-key
// reporting a full configuration file gets, because it runs the same decoder.
//
// The entries are not validated here: a fragment has no server to validate
// against, so the caller merges them into the configuration that owns them and
// validates that. An unknown key inside a fragment is reported through the
// returned warnings rather than silently dropped, which is the same promise a
// configuration file gets.
func LoadFragment(data, name string, opts ValidateOptions) (proxies []ProxyConfig, visitors []VisitorConfig, warnings []string, err error) {
	cfg, err := decodeConfig(data, name, opts)
	if cfg == nil {
		return nil, nil, nil, err
	}
	if len(cfg.Includes) > 0 {
		// The fragments are resolved against the directory of the file that names
		// them, and a fragment has none of its own. LoadString refuses the key for
		// the same reason; here it would be dropped without a word, and the proxies
		// it names would never publish.
		return nil, nil, cfg.Warnings, fmt.Errorf("%s: includes needs a configuration file, because the fragments are resolved relative to the file that names them", name)
	}
	if expandErr := cfg.expandClientEntries(opts); expandErr != nil {
		// A fragment cannot carry includes, so the expansion is the only step that
		// can still fail here.
		return nil, nil, cfg.Warnings, expandErr
	}
	return cfg.Proxies, cfg.Visitors, cfg.Warnings, err
}
