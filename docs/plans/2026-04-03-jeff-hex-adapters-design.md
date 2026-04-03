# Jeff + Hex Adapter Design

## Goal

Add source adapters for Jeff (GSuite agentic loop) and Hex (Rust agentic loop) so ccvault can index their conversation history alongside Claude Code and Codex sessions.

## Jeff Adapter

- **Type name**: `jeff`
- **Session location**: `~/.jeff/sessions/*.jsonl` (pattern: `YYYYMMDD_HHMMSS.jsonl`)
- **Format**: JSONL with `timestamp`, `entry_type`, `conversation_id`, `data` per line
- **Turn mapping**: `user_message` → user turn, `assistant_message` → assistant turn. All other entry types skipped as turns.
- **Tool extraction**: `tool_request` entries map to tool_uses (tool_name from data)
- **Error detection**: `error` entry_type sets HasError on session
- **Model**: From `session_start` data field
- **Project path**: Synthetic `"jeff"` (not a code tool, no CWD concept)

## Hex Adapter

- **Type name**: `hex`
- **Session location**: `~/.hex/sessions/*.json` (one JSON file per conversation)
- **Format**: Single JSON object with `id`, `title`, `created_at`, `updated_at`, `messages[]`, `favorite`
- **Turn mapping**: Each entry in `messages[]` mapped by `role` (user/assistant)
- **Tool extraction**: None in export format
- **Error detection**: None in export format
- **Model**: Not in export format; default to empty
- **Project path**: Synthetic `"hex"` (similar to Jeff)

## Config

```yaml
sources:
  - name: jeff
    type: jeff
    path: ~/.jeff
  - name: hex
    type: hex
    path: ~/.hex
```

## No Other Changes Needed

The existing adapter interface, DB schema (source column), sync layer, and search (source: filter) handle everything. Just two new adapter packages + blank imports in main.go.
