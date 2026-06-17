package main

import "github.com/CampusTech/google2snipe/cmd"

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cmd.Version = version
	cmd.Execute()
}
