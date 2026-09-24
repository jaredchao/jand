// Package docs embeds the agent guide so that jand guide can print it.
package docs

import _ "embed"

// Agent is docs/AGENT.md: the operating contract for agents using jand, as a
// template with __JAND__ and __RELAY_URL__ placeholders and mode sections.
//
//go:embed AGENT.md
var Agent string

// Template is docs/jand-template.md: the structure of a handoff packet.
//
//go:embed jand-template.md
var Template string
