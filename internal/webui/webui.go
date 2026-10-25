// Package webui — собранный клиент, вшитый в laavud. Каталог dist наполняет
// `make web` из web/dist.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS — файлы клиента или nil, если клиент не собран.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
