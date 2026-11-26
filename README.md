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
- **Presets**: create, edit and delete. In the editor, drag ⠿ to reorder,
  use −/+ for sets, ✕ to remove, and the search box to add an exercise, or to
  create a new one on the spot.
- **Exercises**: one box searches the list and adds new ones; it warns when a
  name already exists (ignoring case, accents and punctuation) or looks like a
  typo of one. "Add several at once" takes one name per line. Tap an exercise
  to rename it, add notes, delete it, or **merge** a duplicate into the right
  one (its logged sets move over). Deleting a preset or an exercise never
  deletes logged sessions or sets; they keep the old name.
- **Export**: one CSV for any date range, one row per set:
  `session_id, date, time, weekday, preset, energy, warmup_done, notes, exercise, weight, set, reps`.
  In the iPhone home-screen app, the file opens the iOS share sheet (Save to
  Files, AirDrop, Mail…) instead of a preview you can't leave.

On first run the app seeds `Exercise 1–6`, plus `Day A` (1–3) and `Day B` (4–6).
Rename them on the Exercises and Presets pages.

## Run on the Raspberry Pi

```bash
git clone <your-repo> exercise-logger && cd exercise-logger
echo "TZ=Europe/London" > .env      # your timezone (optionally WEIGHT_UNIT=lb)
docker compose up -d --build
```

Then open `http://<pi-address>:8080`. On iPhone: Share → **Add to Home
Screen** gives you a full-screen "Logger" app with its own icon. The database is the file `./data/logger.db`.
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
| `static/preset_form.js` | preset editor: drag to reorder, sets −/+, search & create |
| `static/exercises.js`, `static/similar.js` | Exercises page search-or-add; duplicate/typo detection (also used by the preset editor) |
| `static/export.js` | share-sheet export for the home-screen app |
| `static/vendor/Sortable.min.js` | [SortableJS](https://github.com/SortableJS/Sortable) 1.15.6 (MIT), the drag & drop |
| `static/manifest.json`, `static/icon-*.png` | home-screen app name and icon |

The templates and CSS are embedded into the binary with `//go:embed`, so the
Docker image is just one file `FROM scratch` (about 18 MB).
