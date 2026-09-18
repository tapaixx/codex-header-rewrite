package main

// Release builds stamp this with the tag version via
// -ldflags "-X main.pluginVersion=<version>". Development builds report dev.
var pluginVersion = "0.0.0-dev"
