# Qdrant Stars Submission Page

A small web app where community members submit content pieces and events for the Qdrant Stars program. Admins can see and delete every submission, and each new submission is posted to Slack.

- **Login:** OIDC (Pocket ID). Members of the `qdrant-admins` group are admins.
- **Submissions:** up to 5 content pieces per month per user (events are unlimited). Deleting a submission within 24 hours of submitting it gives the slot back; after that it does not.
- **AI help:** users can ask for suggestions on a piece before submitting (2 per day). Content submissions with a link also get an AI review in the Slack notification. Both use an OpenRouter agent that reads the link.
- **Storage:** a single embedded [bbolt](https://github.com/etcd-io/bbolt) file, `kv.db`.

## Configuration

Set these environment variables (a `.env` file works for the commands below):

| Variable | Description |
| --- | --- |
| `OIDC_ISSUER` | Pocket ID base URL |
| `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` | OIDC client credentials |
| `BASE_URL` | Public URL of this app, with no trailing slash, e.g. `https://stars.example.com` |
| `OPENROUTER_API_KEY` | OpenRouter API key |
| `SLACK_WEBHOOK_URL` | Slack Workflow webhook. It must define a `message` text variable and an `attachments` text variable (see Attachments) |
| `ATTACHMENTS_DIR` | Optional, where uploaded files are stored. Defaults to `attachments` in the working directory (`/data/attachments` in Docker) |
| `PORT` | Optional, defaults to `8080` |

In Pocket ID, allow the callback URL `<BASE_URL>/auth/callback`, and enable the `groups` scope so admins are recognized.

## Run

```bash
# locally (Go 1.26+)
set -a; source .env; set +a
go run .

# with Docker: kv.db and the attachments live in /data, so mount a volume there to keep them
docker build -t stars-submission-page .
docker run --rm -p 8080:8080 --env-file .env -v "$PWD/data:/data" stars-submission-page
```

Then open `http://localhost:8080` (or your `BASE_URL`). Keep `BASE_URL` in sync with how you reach the app: it sets the login redirect, and session cookies are marked `Secure` when it starts with `https://`.

## Attachments

A submission can carry up to 3 files (PNG, JPG or PDF, 2 MB each), for example to prove that you attended an event. Each user can store 50 MB in total.

- Files are stored on disk in `ATTACHMENTS_DIR` (put it on a volume); only their metadata goes in `kv.db`. Deleting a submission deletes its files.
- The file type is detected from the content, not the file name. Files are served as downloads only.
- **Only admins can open attachments.** The owner sees the file names, not the files.
- **Slack:** the notification webhook gets an extra `attachments` variable, the download links as one text, one link per line (empty when there are none). In Workflow Builder, add it to the webhook trigger as a text variable and use it in the message step. Add the variable before deploying, since the webhook may refuse a payload with a variable it does not define. A link asks the admin to log in first, then opens the file.

## CLI

`qstars` is a command line client for the submission page, for when you would rather not use the browser for everything. It uses the same login as the website.

### Install

Download the archive for your OS and CPU from the [releases page](https://github.com/qdrant-labs/stars-submission-page/releases) (releases named `qstars vX.Y.Z`, tagged `cli@vX.Y.Z`), unpack it, and put the `qstars` binary on your `PATH`. Check the download against `checksums.txt` from the same release.

```bash
# with the GitHub CLI, e.g. Linux on amd64 (use the release tag you want)
gh release download cli@v0.1.0-rc.1 -R qdrant-labs/stars-submission-page -p 'qstars_*_linux_amd64.tar.gz'
tar xzf qstars_*_linux_amd64.tar.gz
sudo mv qstars /usr/local/bin/
qstars --version
```

Or build it from a checkout (Go 1.27+): `go build -o qstars ./cmd/qstars`.

### Log in

```bash
qstars login --server https://stars.example.com
```

This opens your browser on the normal login page. When you finish, the terminal says "Logged in as …". If no browser opens, copy the printed URL into one on the same machine (`--no-browser` only prints it). The session lasts 30 days and is saved in `~/.config/qstars/config.json` (readable by you only). The server is remembered, so later commands need no flags. `QSTARS_URL` can replace `--server`, and `QSTARS_CONFIG` changes where the session is saved.

### Commands

| Command | What it does |
| --- | --- |
| `qstars login [--server URL] [--no-browser]` | Log in through your browser |
| `qstars logout` | End the session on the server and forget it locally |
| `qstars quota` | Show how many content and code submissions you have left this month |
| `qstars list [--all]` | List your submissions, newest first (`--all`: everyone's, admins only) |
| `qstars submit -t TYPE [-u URL] [-d TEXT] [-a FILE]...` | Submit something for review, optionally with attachments |
| `qstars delete ID [-y]` | Delete a submission, after asking (`-y` skips the question) |

`submit` takes `--type` (`content`, `code`, `event` or `other`; required), an optional `--url`, and the description from `--description`, or from standard input when it is piped in:

```bash
qstars submit -t content -u https://example.com/post -d "My post about hybrid search"
qstars submit -t event < description.txt
qstars submit -t event -d "Qdrant meetup Berlin" -a badge.png -a ticket.pdf   # with proof of attendance
```

`--attach` (`-a`) can be repeated up to 3 times. Files are checked before upload (PNG, JPG or PDF, at most 2 MB each).

Limits are the same as on the website: 5 content and 10 code submissions per month, and deleting within 24 hours gives the slot back. `qstars <command> --help` shows all options.

## Test

```bash
go test -race ./...
```
