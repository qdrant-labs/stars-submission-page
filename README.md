# Qdrant Stars Submission Page

A small web app where community members submit content pieces and events for the Qdrant Stars program. Admins can see and delete every submission, and each new submission is posted to Slack.

- **Login:** OIDC (Pocket ID). Members of the `qdrant-admins` group are admins.
- **Submissions:** up to 5 content pieces per month per user (events are unlimited). Deleting a submission does not give the slot back.
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
| `SLACK_WEBHOOK_URL` | Slack Workflow webhook (it must accept a `message` variable) |
| `PORT` | Optional, defaults to `8080` |

In Pocket ID, allow the callback URL `<BASE_URL>/auth/callback`, and enable the `groups` scope so admins are recognized.

## Run

```bash
# locally (Go 1.26+)
set -a; source .env; set +a
go run .

# with Docker
touch kv.db   # the database lives next to the binary, so mount it to keep your data
docker build -t stars-submission-page .
docker run --rm -p 8080:8080 --env-file .env -v "$PWD/kv.db:/app/kv.db" stars-submission-page
```

Then open `http://localhost:8080` (or your `BASE_URL`). Keep `BASE_URL` in sync with how you reach the app: it sets the login redirect, and session cookies are marked `Secure` when it starts with `https://`.

## Test

```bash
go test -race ./...
```
