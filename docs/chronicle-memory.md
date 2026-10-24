# Chronicle / AgentsView Memory in MCPX

Created: 2026-10-24 | Updated: 2026-10-24 | Version: 1.0

Use `mcp_tool` with the configured `agentsview-memory` server for conversation
history. Its memory profile exposes only `search_content`, `get_messages`, and
`get_memory_status`, each annotated read-only. `observe(view=history)` reads
MCPX operation events and does not search the conversation archive.

Configure the upstream with `agentsview mcp --pg --profile memory`, preferably
through Chronicle's shared-network client so the existing connection source is
read only into the child environment. The secret-free configuration example
and verification script live in Chronicle's `ops/shared-network/` directory.
Loading this configuration and replacing binaries are deployment steps; this
change does not change the live MCPX home or restart services.

`history_search`, `history_get`, `history_show`, `history_list`, and
`history_projects` remain compatibility adapters over the Chronicle CLI.
Search defaults to message FTS and includes single-question sessions. Search
supports the backend's integer message cursor; distinct-session pagination is
owned by the native MCP. `history_show` defaults to user/assistant roles and
projects messages to bounded dialogue fields without raw tool-call payloads.
The old `history_peek` and `history_stats` names remain deprecated aliases.

New retrieval behavior belongs in AgentsView and its native MCP. Remove the
compatibility tools only after remote clients pass the native search/read/status
checks and their cached tool catalogs have refreshed. This keeps one search
implementation while allowing an independently reversible rollout.

This branch selectively carries the history implementation from the deployed
history branch. It does not merge that branch's unrelated removals or roll back
mainline browser, edit, revision, or Multica features. General `execute`
authorization is unchanged.
