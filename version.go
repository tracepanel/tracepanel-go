package tracepanel

//go:generate go run ./internal/integrity

// Version is this SDK's version.
const Version = "0.10.0-dev"

// AgentVersion is the agent this SDK runs, and downloads when it isn't installed: of the same minor as Version, a
// patch of its own. After changing it, `go generate` (or `make integrity`) writes the checksums of its binaries,
// which a download must match; a release checks they are of AgentVersion.
const AgentVersion = "0.9.4"
