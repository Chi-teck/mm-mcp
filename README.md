# mm-mcp

A [Model Context Protocol](https://modelcontextprotocol.io) server that lets an MCP host read and write
Mattermost through a personal access token. It runs locally over stdio, exposes 
tools only, and talks to one team via the REST API (no real-time events).

## Install

Prebuilt binaries for linux and macOS (amd64, arm64) are attached to each
[GitHub release](https://github.com/Chi-teck/mm-mcp/releases), with
`checksums.txt`. Or build from source, which requires Go 1.26.7 or newer:

```sh
go install github.com/Chi-teck/mm-mcp/cmd/mm-mcp@latest
```

Put `mm-mcp` on your `PATH` or reference it by absolute path. `mm-mcp --version`
prints the version.

## Configuration

Environment variables only, passed in by the MCP host.

| Variable              | Required | Meaning                                                                                                                               |
|-----------------------|----------|---------------------------------------------------------------------------------------------------------------------------------------|
| `MM_MCP_URL`          | yes      | Server URL, e.g. `https://mattermost.example.com`.                                                                                    |
| `MM_MCP_TOKEN`        | yes      | Personal access token. Never logged or echoed.                                                                                        |
| `MM_MCP_TEAM`         | yes      | Team name or 26-char team id.                                                                                                         |
| `MM_MCP_DOWNLOAD_DIR` | no       | Existing, writable directory where `get_file` saves attachments; never created. Unset: `get_file` returns an error.                   |
| `MM_MCP_UPLOAD_ROOT`  | no       | `create_post` attachments must resolve inside it. Default: the working directory; attachments are disabled if that is `/` or `$HOME`. |

Relative paths resolve against the server's working directory. At startup the
server validates the variables, checks the token and resolves the team (10 s
timeout); on failure it prints every problem to stderr in one line and exits 1.
stdout carries MCP frames only.

GUI hosts such as Claude Desktop usually start servers in `/` or `$HOME`, so set
`MM_MCP_UPLOAD_ROOT` explicitly there if you need attachments.

Example host config (file location and top-level key vary by host;
`${MM_MCP_TOKEN}` works only where the host expands variables):

```json
{
  "mcpServers": {
    "mattermost": {
      "command": "mm-mcp",
      "env": {
        "MM_MCP_URL": "https://mattermost.example.com",
        "MM_MCP_TOKEN": "${MM_MCP_TOKEN}",
        "MM_MCP_TEAM": "my-team",
        "MM_MCP_DOWNLOAD_DIR": "/home/me/mm-files",
        "MM_MCP_UPLOAD_ROOT": "/home/me/mm-uploads"
      }
    }
  }
}
```

## Tools

Each tool carries MCP annotations (`readOnlyHint`, `destructiveHint`, …) that
hosts use to decide what to prompt for.

| Tool              | Kind                        | What it does                                                                  |
|-------------------|-----------------------------|-------------------------------------------------------------------------------|
| `list_channels`   | read-only                   | List your channels in the team.                                               |
| `read_posts`      | read-only                   | Latest posts, posts since a time or before a post, pinned posts, or a thread. |
| `get_post`        | read-only                   | One post by id, with reactions and attachments.                               |
| `search`          | read-only                   | Search posts or files by keyword.                                             |
| `list_members`    | read-only                   | List a channel's members, or fuzzy-match usernames.                           |
| `get_file`        | write (local disk)          | Download an attachment into `MM_MCP_DOWNLOAD_DIR`.                            |
| `follow_thread`   | write                       | Follow a thread.                                                              |
| `unfollow_thread` | write                       | Stop following a thread.                                                      |
| `create_post`     | write                       | Post or reply, with attachments, now or scheduled.                            |
| `react`           | write, destructive (remove) | Add or remove an emoji reaction.                                              |
| `edit_post`       | write, destructive          | Edit or delete one of your own posts.                                         |
| `dm`              | write                       | Open (or reuse) a direct-message channel.                                     |
| `api`             | write, destructive          | Raw request to any `/api/v4` endpoint; fallback when no dedicated tool fits.  |

## Security

The model reads untrusted channel content, and any message can try to steer it.

- **The token is the blast radius.** The server has the token user's full
  rights, and `api` reaches every endpoint, admin ones included. Use a
  least-privileged account.
- **No write confirmation.** Gating is the host's job, e.g. in Claude Code's
  `settings.json`:
  ```json
  {
    "permissions": {
      "ask": [
        "mcp__mattermost__create_post", 
        "mcp__mattermost__edit_post",
        "mcp__mattermost__react",
        "mcp__mattermost__dm"
      ],
      "deny": ["mcp__mattermost__api"]
    }
  }
  ```

- **Upload confinement is not exfiltration control.** Symlinks cannot escape
  the upload root, but the model can paste secrets into message text. Keep the
  root narrow and not writable by untrusted processes.
- `api` is confined to `/api/v4/`; `api` and `get_file` never follow redirects;
  `get_file` refuses files over 256 MiB.

## Development

```sh
go vet ./...
go test ./...
golangci-lint run
```
