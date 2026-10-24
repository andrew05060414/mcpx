# Chronicle / AgentsView Memory in MCPX

## Architecture
- Upstream MCP (preferred): `agentsview-memory` with `search_content`, `get_messages`, `get_memory_status` via `mcp_tool`.
- Compatibility layer: `history_search`, `history_get`, `history_show`, `history_list`, `history_projects` via local `chronicle` CLI.
- Deprecated aliases: `history_peek` (-> `history_get`), `history_stats` (-> `history_projects`).
- Local observation history (`observe view=history`) observes MCPX events, not the cross-session archive.
