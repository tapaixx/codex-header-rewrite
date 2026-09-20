package main

// Release builds stamp this with the tag version via
// -ldflags "-X main.pluginVersion=<version>". Development builds report dev.
var pluginVersion = "0.0.0-dev"

// pluginLogo is the mark shown beside this plugin in management clients. It
// points at the artwork in the repository so the store and the installed
// entry cannot drift from what the panel draws.
const pluginLogo = "https://raw.githubusercontent.com/tapaixx/codex-header-rewrite/main/web/icon.svg"
