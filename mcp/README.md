# forkbox-mcp

An MCP server that gives agents Forkbox sandboxes as tools, over stdio:

| tool | what it does |
| --- | --- |
| `create_sandbox(template, ttl_seconds)` | a new sandbox from a template (`python`, `node`) |
| `exec(sandbox_id, command, timeout_seconds)` | run a shell command; exit code, stdout, stderr (long output cut in the middle, with a hint) |
| `read_file(sandbox_id, path, offset, max_bytes)` | read a file, in parts for large files |
| `write_file(sandbox_id, path, content)` | create or replace a file |
| `fork(sandbox_id, n)` | copy a running sandbox into n independent ones |
| `checkpoint(sandbox_id, name)` | save files, processes and memory under a name |
| `rewind(sandbox_id, name)` | go back to a checkpoint |
| `list_sandboxes()` | what exists |
| `destroy(sandbox_id)` | remove a sandbox |

## Install

On the Forkbox host, with `forkboxd` running:

```sh
python3.12 -m venv /opt/forkbox-mcp
/opt/forkbox-mcp/bin/pip install ./sdk/python ./mcp
```

The server connects to `forkboxd` like the SDK does: `$FORKBOX_SOCKET`
(default `/var/lib/forkbox/run/forkboxd.sock`, owner-only), or a TCP listener
with `$FORKBOX_ADDR` and `$FORKBOX_TOKEN`.

## Client configuration

Desktop MCP clients and Cursor (`~/.cursor/mcp.json`) take the standard
`mcpServers` entry:

```json
{
  "mcpServers": {
    "forkbox": {
      "command": "/opt/forkbox-mcp/bin/forkbox-mcp",
      "env": { "FORKBOX_ADDR": "forkbox-host:7443", "FORKBOX_TOKEN": "..." }
    }
  }
}
```

On the Forkbox host itself, drop `env` and run the client as the daemon's
user (the socket is owner-only), or set `FORKBOX_SOCKET`.

## Tests

```sh
pip install -e 'sdk/python[test]' -e 'mcp[test]'
pytest mcp
```
