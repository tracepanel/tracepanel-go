package tracepanel

//go:generate go run ./internal/integrity

// Version is this SDK's version.
const Version = "0.3.0"

// AgentVersion is the agent this SDK runs, and downloads when it isn't installed: released with the SDKs, at the same
// version. After changing it, `go generate` writes the checksums of its downloads into integrity.go.
const AgentVersion = Version
