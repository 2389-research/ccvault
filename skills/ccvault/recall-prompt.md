# Recall: Historical Solution Lookup

## Purpose

Find specific past solutions, decisions, or patterns from conversation history when needed. This is the "how did I solve X?" workflow. Use it when you or the user need prior art before implementing something, when a familiar error resurfaces, or when revisiting a past decision.

## When to Use

- "How did I solve the CORS issue last time?"
- "Find that database migration pattern we used"
- "What approach did we take for authentication?"
- You're about to implement something and want to check for prior art
- An error message appears that was solved in a prior session
- You need to recall a decision rationale or tradeoff discussion

## Workflow

### Step 1: Parse Intent

Extract from the request:
- **Keywords** — error messages, library names, patterns, function names, tool names
- **Project hints** — explicitly mentioned or inferrable from current working directory
- **Time hints** — "last week", "in December", "recently", "a while ago"

### Step 2: Construct Search Query

Build a query using ccvault operators:
```
search_conversations query:"<keywords>" [project:<project>] [after:<date>] [tool:<tool>]
```
For error messages, quote the most distinctive fragment. Start narrow (more operators) and broaden if no results. See the narrowing strategy below.

### Step 3: Evaluate Results

For each promising hit (judge by snippet relevance):
```
get_session_summary session_id:<hit_session_id>
```
Verify the session actually covers the topic. Check `first_user_msg` and `tools_used` to rule out false positives before spending context on full turns. One caveat on `tools_used`: when the response carries `unreadable_turns`, the tool counts are an undercount, so a tool missing from the list is not grounds to rule the session out — fall back to `first_user_msg` and the snippet.

### Step 4: Extract the Solution

For confirmed matches, pull the relevant assistant turns:
```
get_turns session_id:<session_id> type:assistant offset:<relevant_area>
```
For small sessions (<100 turns), `get_session` is acceptable. Look for the turns that contain the actual fix, code, or decision — not the exploratory discussion around it.

### Step 5: Present Findings

Always include:
- **Session ID** — for future reference
- **Date and project** — so the user can place it in context
- **The solution** — extracted code, command, or decision
- **Confidence** — exact match vs. related/tangential topic

## Search Narrowing Strategy

Waterfall approach. Start tight, loosen one filter per round until you get hits:

```
Round 1: project:X tool:Y "exact error message"
Round 2: project:X "error message"              # drop tool filter
Round 3: "error message"                         # drop project filter
Round 4: "distinctive phrase from error"         # simplify terms
```

Stop as soon as you get relevant results. If you reach Round 4 with nothing, move to the no-results protocol.

## Multiple Results

When several sessions match:
- Present the most recent match first — it likely reflects the latest thinking
- Note if the same problem was solved differently across sessions (the approach may have evolved)
- Highlight which session had the most comprehensive or complete solution
- Group by session; don't interleave turns from different sessions

## No Results

When nothing is found, report honestly and suggest next steps:
1. **Try different terms** — synonyms, related concepts, the library name instead of the error
2. **Check archive coverage** — run `get_stats` and look at `first_activity` / `last_activity` to see if the solution predates the archive
3. **Verify project name** — run `list_projects` to confirm the project name matches what ccvault has indexed
4. **Check sync status** — if `last_activity` in stats is stale, the archive may need a refresh
5. **Suggest manual recall** — the user may remember details that improve the search query
