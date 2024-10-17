package static

import (
	"embed"
	"net/http"
)

//go:embed static
var base embed.FS

func NewHandler() http.Handler {
	return http.FileServerFS(base)
}
