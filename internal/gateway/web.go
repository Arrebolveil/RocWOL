package gateway

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed web/*
var resources embed.FS

func (g *Gateway) page(w http.ResponseWriter, name string, status ...int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	switch {
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	b, e := resources.ReadFile("web/" + name)
	if e != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	if len(status) > 0 {
		w.WriteHeader(status[0])
	}
	_, _ = w.Write(b)
}
