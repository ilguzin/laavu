// Package deploy — шаблоны файлов, которые ставит laavu install и laavud priv.
package deploy

import (
	"bytes"
	"embed"
	"text/template"
)

//go:embed tmux.conf.tmpl Caddyfile.tmpl reserved tmpfiles.conf systemd agent
var Files embed.FS

// Render заполняет шаблон из Files.
func Render(name string, data any) ([]byte, error) {
	b, err := Files.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return RenderBytes(name, b, data)
}

// RenderBytes заполняет шаблон из текста.
func RenderBytes(name string, b []byte, data any) ([]byte, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(string(b))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
