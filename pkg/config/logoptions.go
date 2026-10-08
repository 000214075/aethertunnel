package config

import "github.com/aethertunnel/aethertunnel/pkg/logging"

// Options converts the [log] section into the form the logging package takes.
//
// The conversion lives here rather than in pkg/logging because that package is
// imported by packages config itself depends on (pkg/dht, pkg/discovery), so it
// must not import config back.
func (l LogConfig) Options() logging.Options {
	return logging.Options{
		To:                l.To,
		MaxDays:           l.MaxDays,
		Level:             l.Level,
		DisableTimestamp:  l.DisableTimestamp,
		DisablePrintColor: l.DisablePrintColor,
	}
}
