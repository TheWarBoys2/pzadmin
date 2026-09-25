package main

import "embed"

// webAssets carries the browser interface. Embedding it keeps PZAdmin a single
// static binary with no runtime file dependencies.
//
//go:embed web/index.html web/app.css web/app.js web/favicon.svg
var webAssets embed.FS
