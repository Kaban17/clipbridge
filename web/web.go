// Package web holds the browser client, embedded into the binary.
package web

import _ "embed"

// Page is the whole browser client: one HTML file with inline CSS and JS.
//
//go:embed index.html
var Page []byte
