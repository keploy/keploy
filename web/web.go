package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed public
var content embed.FS

func Handler() http.Handler {
	fsys := fs.FS(content)
	contentStatic, _ := fs.Sub(fsys, "public")
	return http.FileServer(http.FS(contentStatic))
}
