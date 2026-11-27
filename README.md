# criterion-bridge

Watch the [Criterion Channel](https://www.criterionchannel.com/)'s 24/7 live stream in [Channels DVR](https://getchannels.com/) as a custom channel, with a full guide built from Criterion's own schedule.

It signs in with **your own Criterion Channel subscription**, just like criterionchannel.com does, and asks Criterion for a fresh stream every time you tune in. No subscription, no stream. It doesn't download or alter anything; Channels plays the stream exactly as Criterion serves it.

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
| `TMDB_API_KEY` | A TMDB key, for movie posters instead of title cards ([see below](#portrait-posters)) | none |
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

Out of the box, each film's guide art is Criterion's widescreen title card. If you'd rather have the movie posters Channels DVR shows everywhere else, give the bridge a free TMDB key and it'll look them up.

### Getting a TMDB key

1. Sign in to [TMDB](https://www.themoviedb.org/), or create a free account.
1. Go to the [developer sign-up](https://www.themoviedb.org/subscribe/developer) and fill in the form. For **Type of Use**, pick **Personal**. For the URL, this repo's is fine.
1. Once you've agreed to the terms, your key is under [Settings > API](https://www.themoviedb.org/settings/api). Either the **API Key** or the longer **API Read Access Token** works.
1. Add it to `.env` and restart the bridge:

```bash
echo 'TMDB_API_KEY=your-key' >> .env
docker compose up -d
```

### How posters are picked

The bridge searches TMDB for each film by title and release year and uses the top result. If that finds nothing, it tries a year either side, since Criterion and TMDB sometimes date a film by different releases. Then it tries the title alone, and uses the result only if it's the sole match, so a remake can't slip in. Anything still unmatched keeps its title card. Each film is only looked up once, so expect about 150 searches when the bridge starts and a handful a day after that.

If TMDB rejects the key, the guide keeps working with title cards and the bridge's log says so.

Channels saves a recording's art when you schedule it, so anything scheduled before you added the key keeps the title card. Cancel it and schedule it again from the guide to get the poster.

### Attribution

[![The Movie Database (TMDB)](https://www.themoviedb.org/assets/v4/logos/v2/blue_short-8e7b30f73a4020692ccca9c88bafe5dcb6f8a62a4c6bc55cd9ba82bb2cd95f6c.svg)](https://www.themoviedb.org/)

Posters come from [The Movie Database (TMDB)](https://www.themoviedb.org/). This product uses the TMDB API but is not endorsed or certified by TMDB.

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
