# Exercise Logger

A small, self-hosted web app for logging workout sessions. It's written in Go
with only the standard library plus a pure-Go SQLite driver, and it runs on a
Raspberry Pi with Docker Compose.

## How it's organised

```
Preset   (Day A)          ──  an ordered list of exercises, each with a target number of sets
Session  (Mon 16 Nov)     ──  date & time, energy 0–10, warm-up ✓, notes
 └─ Exercise (Squat)      ──  working weight
     └─ Set 1, 2, 3 …     ──  reps
```

## Pages

- **Log**: in order: date & time, warm-up, energy, session (the day after
  last session's is pre-selected; switching doesn't reload the page), then
  each exercise's weight and reps per set, notes, and the Log button, which
  stays on screen while you scroll. Last time's weight is pre-filled, last
  time's reps show in grey, and **↺ Same** copies them in with one tap. Leave
  an exercise's reps empty to skip it.
- **Progress**: your paper table, digitised. Rows are exercises, columns are
  sessions (oldest → newest), each cell is weight and reps, with ▲/▼ when the
  weight changed. Energy and warm-up are at the bottom.
- **Sessions**: list, edit, delete.
- **Presets**: create, edit (choose exercises, order, sets) and delete.
- **Exercises**: create, rename, delete. Renaming keeps the history.
  Deleting a preset or an exercise never deletes logged sessions or sets;
  they keep the old name.
- **Export**: one CSV for any date range, one row per set:
  `session_id, date, time, weekday, preset, energy, warmup_done, notes, exercise, weight, set, reps`.

On first run the app seeds `Exercise 1–6`, plus `Day A` (1–3) and `Day B` (4–6).
Rename them on the Exercises and Presets pages.

## Run on the Raspberry Pi

```bash
git clone <your-repo> exercise-logger && cd exercise-logger
echo "TZ=Europe/London" > .env      # your timezone (optionally WEIGHT_UNIT=lb)
docker compose up -d --build
```

Then open `http://<pi-address>:8080`. The database is the file `./data/logger.db`.
To back up, copy the `data/` folder.

Upgrade: `git pull && docker compose up -d --build`. Schema changes are
applied automatically on startup (see `migrations` in `store.go`).

## Run locally without Docker

```bash
go run .      # http://localhost:8080
go test ./... # store tests
```

| Env var       | Default          | Meaning                     |
|---------------|------------------|-----------------------------|
| `ADDR`        | `:8080`          | listen address              |
| `DB_PATH`     | `data/logger.db` | SQLite file location        |
| `TZ`          | system           | timezone for date/times     |
| `WEIGHT_UNIT` | `kg`             | label shown next to weights |

## Code tour

| File | What's in it |
|------|--------------|
| `main.go` | config from env vars, HTTP server, graceful shutdown |
| `store.go` | models, schema migrations, every database query |
| `handlers.go` | routes (Go 1.22+ `"METHOD /path/{id}"` patterns), form parsing, progress grid, CSV export |
| `store_test.go` | round-trip test of sessions, sets, filters and delete rules |
| `templates/` | `html/template` pages; `layout.html` wraps each page, `partials.html` holds the session form and table |
| `static/style.css` | styling (dark mode included) |

The templates and CSS are embedded into the binary with `//go:embed`, so the
Docker image is just one file `FROM scratch` (about 18 MB).
