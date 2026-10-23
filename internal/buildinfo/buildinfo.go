// Package buildinfo holds what the build stamps into the program.
package buildinfo

// Version is the release, such as 2.1.0, set at build time with
// -ldflags "-X github.com/xuhuanhello/gamefleet/internal/buildinfo.Version=2.1.0".
// A build from source says dev.
var Version = "dev"
