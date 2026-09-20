// Package web embeds the dashboard so a released binary serves its own UI with no
// files next to it and no dependency on the working directory.
//
// The v1 dashboard was read from disk with two mutually inconsistent relative
// paths ("../../web/dashboard" for the file server and "web/dashboard/index.html"
// for the page handlers), so at most one of them resolved, and the release
// archives did not ship the assets at all.
package web

import "embed"

//go:embed dashboard
var FS embed.FS

// Dashboard is the sub-filesystem served at "/".
const Dashboard = "dashboard"
