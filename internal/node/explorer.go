package node

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
	"path"
	"strings"
	"sync"
)

// The block explorer: a page and its assets, embedded, that read the node's
// own /v1 API in the browser. Its pages sit at the reference explorer's
// paths (/, /blocks, /blocks/{id}, /transactions/{id}, /contracts/{id},
// /mempool) and two more of its own (/network, /insights).
//
//go:embed explorer/*
var explorerFiles embed.FS

var explorerPages = []string{"/{$}", "/blocks", "/blocks/{id}", "/transactions/{id}", "/contracts/{id}",
	"/mempool", "/network", "/insights"}

// explorerCSP lets the page load only its own scripts, styles and data.
const explorerCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// explorerIndex is the page with its assets' URLs carrying a hash of their
// contents, so a browser fetches new assets as soon as they change.
var explorerIndex = sync.OnceValue(func() []byte {
	page, err := explorerFiles.ReadFile("explorer/index.html")
	if err != nil {
		return nil
	}
	for _, name := range []string{"app.js", "style.css", "logo.svg"} {
		b, err := explorerFiles.ReadFile("explorer/" + name)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(b)
		url := "/assets/" + name
		page = bytes.ReplaceAll(page, []byte(`"`+url+`"`), []byte(`"`+url+"?v="+hex.EncodeToString(sum[:6])+`"`))
	}
	return page
})

func (n *Node) explorerPage(w http.ResponseWriter, r *http.Request) {
	b := explorerIndex()
	if b == nil {
		http.Error(w, "explorer missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", explorerCSP)
	w.Write(b)
}

var assetTypes = map[string]string{".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8",
	".svg": "image/svg+xml"}

func (n *Node) explorerAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	typ, ok := assetTypes[path.Ext(name)]
	if !ok || strings.ContainsAny(name, "/\\") || name == "index.html" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "asset not found"})
		return
	}
	b, err := explorerFiles.ReadFile("explorer/" + name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "asset not found"})
		return
	}
	w.Header().Set("Content-Type", typ)
	// Versioned by the page's URLs, so they may be kept a long time.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(b)
}
