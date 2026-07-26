// Package web carries the browser application: HTML, CSS, JavaScript, icons and the
// service worker, compiled into the binary.
//
// The embed directive lives here, beside the files, because go:embed cannot reach
// outside its own package directory — which is also why the frontend keeps its
// natural place at the top of the repository instead of being buried under cmd/.
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html manifest.json style.css sw.js js icons
var files embed.FS

// FS is the browser application, rooted so that "index.html" is at the top.
func FS() fs.FS { return files }
