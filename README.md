# criterion-bridge

Serves the [Criterion Channel](https://www.criterionchannel.com/)'s 24/7 live stream to [Channels DVR](https://getchannels.com/) as a custom channel, with a full guide built from Criterion's own schedule.

It signs in with **your own Criterion Channel subscription**, the same way criterionchannel.com does in a browser, and asks Criterion for the stream each time the channel is tuned. If your subscription ends, it stops working. It doesn't record, download, or strip anything; Channels DVR plays the stream as Criterion serves it.

## Running it

From a clone of this repo:

```bash
cp compose.example.yaml compose.yaml
cat > .env <<'EOF'
CRITERION_EMAIL=you@example.com
CRITERION_PASSWORD=your-password
EOF
docker compose up -d --build
```

To update, pull and rebuild:

```bash
git pull && docker compose up -d --build
```

| Variable | Description | Default |
|---|---|---|
| `CRITERION_EMAIL` | Your Criterion Channel email | required |
| `CRITERION_PASSWORD` | Your Criterion Channel password | required |
| `CRITERION_BRIDGE_PORT` | The host port, set in `.env` for compose | `8080` |
| `LISTEN_ADDR` | Where to serve inside the container | `:8080` |

Keep `.env` private. It's gitignored, and the bridge never logs your email or password.

## Adding it to Channels DVR

1. Go to **Settings** > **Custom Channels** > **Add Source** > **M3U Playlist**.
1. Set the stream format to **HLS** and the URL to `http://<host>:8080/playlist.m3u`.
1. Set **XMLTV Guide Data** to `http://<host>:8080/guide.xml`.

The guide covers the next several days of films, with descriptions, genres and ratings, so you can browse and record by film.

## How sign-in works

The bridge doesn't sign in until the channel is first tuned. It then signs in to Criterion's login page with your email and password and keeps the session for 25 days (Criterion's sessions last 30). If Criterion ends the session early, the next tune signs in again.

It's careful not to hammer Criterion's login, which could get your account locked:

- If Criterion rejects your email or password, it doesn't try again until you fix them and restart it.
- After any other sign-in failure, it waits 30 minutes before trying again.
- It can't complete two-factor authentication. If your account uses it, the bridge reports that and stops.

When sign-in isn't working, tuning the channel fails and the bridge's log says why.

## Endpoints

| Path | Description |
|---|---|
| `/playlist.m3u` | The Criterion 24/7 channel |
| `/guide.xml` | XMLTV guide from Criterion's schedule, refreshed hourly |
| `/play` | Redirects to the live stream, signing in first if needed |
| `/healthz` | Returns 200 while the server is up |
