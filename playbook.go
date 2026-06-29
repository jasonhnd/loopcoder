// Package loopcoder exposes the playbook files bundled into the binary.
package loopcoder

import (
	"embed"
	"io/fs"
)

// bundledPlaybook embeds the real repository-root conductor entrypoints. This
// file intentionally lives at the module root because Go embed patterns cannot
// reference parent directories from an internal package.
//
//go:embed SKILL.md AGENTS.md
var bundledPlaybook embed.FS

// PlaybookFS returns the embedded playbook filesystem.
func PlaybookFS() fs.FS {
	return bundledPlaybook
}

// SkillMarkdown returns the embedded Claude Code skill entrypoint.
func SkillMarkdown() ([]byte, error) {
	return fs.ReadFile(bundledPlaybook, "SKILL.md")
}

// AgentsMarkdown returns the embedded Codex AGENTS entrypoint.
func AgentsMarkdown() ([]byte, error) {
	return fs.ReadFile(bundledPlaybook, "AGENTS.md")
}
