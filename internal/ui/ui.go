// Package ui embeds the web dashboard: static HTML, CSS and JavaScript with
// no build step and no third-party code. It reads the controller's /api/*
// endpoints and is read-only.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Handler serves the dashboard's files.
func Handler() http.Handler {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err) // the directory is compiled in; Sub only fails on a bad path
	}
	return http.FileServerFS(sub)
}
