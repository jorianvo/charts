// Package web serves the stylesheet shared by the connector and bridge UIs.
package web

import (
	"embed"
	"net/http"
)

//go:embed static
var static embed.FS

// Static serves the files under /static/.
func Static() http.Handler {
	return http.FileServerFS(static)
}
