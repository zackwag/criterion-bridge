# criterion-bridge

Serves the [Criterion Channel](https://www.criterionchannel.com/)'s 24/7 live stream to [Channels DVR](https://getchannels.com/) as a custom channel, with a full guide built from Criterion's own schedule.

It signs in with **your own Criterion Channel subscription**, the same way criterionchannel.com does in a browser, and asks Criterion for the stream each time the channel is tuned. If your subscription ends, it stops working. It doesn't record, download, or strip anything; Channels DVR plays the stream as Criterion serves it.

## Running it

The image is [`zackwag/criterion-bridge`](https://hub.docker.com/r/zackwag/criterion-bridge) on Docker Hub, for `linux/amd64` and `linux/arm64`. Save [`compose.example.yaml`](compose.example.yaml) as `compose.yaml`, put your sign-in in a `.env` file next to it, and start it:

```bash
cat > .env <<'EOF'
CRITERION_EMAIL=you@example.com
CRITERION_PASSWORD=your-password
EOF
docker compose up -d
```

To update:

```bash
docker compose pull && docker compose up -d
```

| Variable | Description | Default |
|---|---|---|
| `CRITERION_EMAIL` | Your Criterion Channel email | required |
| `CRITERION_PASSWORD` | Your Criterion Channel password | required |
| `TMDB_API_KEY` | A TMDB API key or read access token, for portrait posters (see below) | none |
| `CRITERION_BRIDGE_PORT` | The host port, set in `.env` for compose | `8080` |
| `LISTEN_ADDR` | Where to serve inside the container | `:8080` |

Keep `.env` private. It's gitignored, and the bridge never logs your email or password.

## Adding it to Channels DVR

1. Go to **Settings** > **Custom Channels** > **Add Source** > **M3U Playlist**.
1. Set the stream format to **HLS** and the URL to `http://<host>:8080/playlist.m3u`.
1. Set **XMLTV Guide Data** to `http://<host>:8080/guide.xml`.
1. Leave **Prefer channel logos from M3U** selected, so the channel shows Criterion's logo.

The guide covers the next several days of films, with descriptions, release dates, genres, ratings and artwork, so you can browse and record by film.

## Portrait posters

Criterion's own artwork for each film is a widescreen title card. To get the portrait posters Channels DVR shows for other movies, set `TMDB_API_KEY` to a free [TMDB API key](https://www.themoviedb.org/settings/api) (the API key or the read access token both work). The bridge then searches TMDB for each film by its title and release year and uses the top result's poster, keeping Criterion's title card for any film TMDB doesn't have. It looks each film up once: about 140 searches when it starts, then only the films Criterion adds to the schedule.

Channels saves a recording's artwork when you schedule it, so recordings scheduled before you set the key keep the title card. Schedule them again from the guide to pick up the poster.

This product uses the TMDB API but is not endorsed or certified by TMDB.

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
