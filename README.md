# gesso

Deploy a directory of static files to a Cloudflare Worker. One Go binary, no
dependencies, no build config, no Node.

Used in production, deploying a live static site.

## Why

Cloudflare's position is that new projects should start on Workers, and that
Pages is supported but gets no new investment. Workers also happens to be the
better target for static files, since it costs the same and brings Cron
Triggers, Queues, Durable Objects and real logs if the site ever needs them.

Wrangler is the official deployer, and it is the right tool for a project with a
build step, bindings and local dev. For one static directory it puts a Node
runtime and its whole dependency tree in the deploy path, plus a lockfile to
keep in step and a package manager on every machine that deploys.

Uploading a static site is three HTTP calls. Paying for that with a runtime
install is a bad trade, so gesso is those three calls as one static binary:
6.4 MB, statically linked, no dependencies, nothing to install, nothing to
audit, nothing that changes underneath you between deploys.

That is the size argument, stated honestly: 6.4 MB is not small in absolute
terms, and Go's runtime with `net/http` is 3.4 MB of it before any of this code.
It is one file with no install step, against a Node runtime and its dependency
tree.

Files are addressed by content, so a redeploy of an unchanged directory uploads
nothing: two requests, no asset bytes.

## Install

```
go install github.com/zipadoodlez/gesso@latest
```

Or build a static binary and put it on your PATH:

```
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o gesso .
install -m 755 gesso ~/.local/bin/
```

The flags are what get it to 6.4 MB. `go install` above skips them, so it
produces the larger, dynamically linked build.
```

## Usage

```
gesso -name <worker> [-dry-run] [-compat YYYY-MM-DD] <directory>
gesso -h
```

`-name` is the Worker the version is uploaded to. `-h`, `-help` and `--help`
all print the same page.

```
gesso -name example-site ./public
gesso -name example-site -dry-run ./public
gesso -name example-site -compat 2026-11-20 ./public
```

`-name` is required, including with `-dry-run`. `-compat` defaults to today,
UTC. `-dry-run` prints the files, their content keys, sizes and types, and
anything skipped, then exits without reading credentials or touching the
network.

## Credentials

Two values, read from the environment first, then from `~/.config/gesso/`:

| Variable | File | Value |
|---|---|---|
| `CLOUDFLARE_API_TOKEN` | `token` | **Account** · **Workers Scripts** · **Edit** |
| `CLOUDFLARE_ACCOUNT_ID` | `account-id` | the 32-character account id |

**The token.** At <https://dash.cloudflare.com/profile/api-tokens>, choose
Create Token, then Get started under Create Custom Token. Give it Account ·
Workers Scripts · Edit, scoped to your account. Do not use the *Edit Cloudflare
Workers* template: it also grants Workers Routes, KV, R2, Tail, account settings
and user details, none of which a static deploy needs. The value is shown once.

**The account id.** In Workers & Pages, copy Account ID from Account Details. It
is also the first path segment of `dash.cloudflare.com/<account-id>`.

Paste the token into `~/.config/gesso/token` and the account id into
`~/.config/gesso/account-id`. Bare values, nothing else — no `KEY=`, no
`Bearer`, no quotes. Trailing whitespace is trimmed.

gesso never writes to disk. Both files are in `.gitignore` in case one is ever
put next to the code.

## What it does

Three steps per deploy, and so N+2 HTTP calls for N files the store does not
already hold.

1. **Session.** `POST /accounts/{id}/workers/scripts/{name}/assets-upload-session`
   with the manifest, `{"/index.html": {"hash": ..., "size": ...}}`. The reply
   carries a JWT and `buckets`: the hashes the asset store does not already
   have. This is also the check step, so an unchanged site uploads nothing.
2. **Upload.** One `POST /accounts/{id}/workers/assets/upload/{hash}` per
   missing asset, authenticated with the session JWT. The last response carries
   the completion JWT.
3. **Version.** `PUT /accounts/{id}/workers/scripts/{name}` as multipart
   metadata: `assets.jwt`, `assets.config._headers`, `compatibility_date`.

No Worker script is sent, so the version is assets only.

## Asset keys

`sha256(base64(contents) + extension)`, the extension without its dot, cut to
the first 32 hex characters.

Cloudflare's own client uses BLAKE3 for the same purpose. The server keys on the
string alone, so any stable per-extension digest works; the extension is
included because identical bytes at a `.html` and a `.txt` path are two
different assets. `assetKey` in `main.go` marks the place to swap to BLAKE3 if a
deploy ever proves the API wants it.

## Ignored files

Always skipped: dotfiles and dot-directories, `__pycache__`, `node_modules`,
`*.pyc`.

One [path.Match](https://pkg.go.dev/path#Match) pattern per line in
`<directory>/.siteignore`, `#` for comments. Patterns are matched against the
whole relative path and against the base name.

`.siteignore` is to the upload what `.gitignore` is to a commit: name what stays
on the machine, and everything else goes. It is not `.gitignore` and never reads
one, so a directory that has a `.gitignore` still needs its own `.siteignore`.

`_headers` and `_redirects` are never uploaded as files.

A top-level `_headers` is passed to Workers as configuration, so its rules
apply.

A top-level `_redirects` is dropped, and no redirect config is sent with it.
Redirect rules in that file do not take effect through gesso; use the Worker's
own routing when you need them.

## Refused

A directory containing `functions/`, `_worker.js` or `_routes.json` needs a
Worker script. gesso serves static assets only, so it stops rather than
deploying something that half works.

A single file over 25 MiB, or more than 20,000 files, is an error.

## What it does not do

- No Worker script, no functions, no SSR.
- No `_redirects` config. See above.
- No git integration, no branch deploys, no rollback, no preview URLs.
- Not Cloudflare Pages.

## License

MIT. See [LICENSE](LICENSE).
