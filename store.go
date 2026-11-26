package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// dtLayout is how session timestamps are stored in the DB. It is exactly the
// format an HTML <input type="datetime-local"> sends and expects, and it sorts
// correctly as plain text.
const dtLayout = "2006-01-02T15:04"

// ---------------------------------------------------------------------------
// Models
//
//   Preset ──< PresetExercise >── Exercise        (the plan:  "Day A = squat, bench, row")
//   Session ──< SessionExercise ──< set (reps)    (what you actually did)
// ---------------------------------------------------------------------------

type Exercise struct {
	ID    int64
	Name  string
	Notes string
}

// PresetExercise is an exercise as planned in a preset, with a target number
// of sets (how many reps boxes the log form shows).
type PresetExercise struct {
	Exercise
	Sets int
}

// Preset is a named, ordered list of exercises, e.g. "Day A".
type Preset struct {
	ID        int64
	Name      string
	Exercises []PresetExercise // in workout order
}

// ExerciseNames returns the preset's exercises as a comma-separated string.
func (p Preset) ExerciseNames() string {
	names := make([]string, len(p.Exercises))
	for i, e := range p.Exercises {
		names[i] = e.Name
	}
	return strings.Join(names, ", ")
}

// Session is one logged workout.
type Session struct {
	ID          int64
	PresetID    int64 // 0 if the preset has since been deleted
	PresetName  string
	PerformedAt time.Time
	Energy      int // 0..10
	WarmupDone  bool
	Notes       string
	Exercises   []SessionExercise // in the order they were done
}

// SessionExercise is one exercise performed in a session: the working weight
// and the reps achieved in each set.
type SessionExercise struct {
	ExerciseID int64 // 0 if the exercise has since been deleted
	Name       string
	Weight     sql.NullFloat64 // NULL = bodyweight / not recorded
	Reps       []int           // one entry per set, in order

	rowID int64 // session_exercises.id, used while loading
}

// Deleted reports whether this entry's exercise no longer exists.
func (se SessionExercise) Deleted() bool { return se.ExerciseID == 0 }

// WeightText formats the weight without trailing zeros ("60", "62.5"), or "".
func (se SessionExercise) WeightText() string {
	if !se.Weight.Valid {
		return ""
	}
	return strconv.FormatFloat(se.Weight.Float64, 'f', -1, 64)
}

// RepsText formats the reps like you'd write them on paper: "8/8/7".
func (se SessionExercise) RepsText() string {
	parts := make([]string, len(se.Reps))
	for i, r := range se.Reps {
		parts[i] = strconv.Itoa(r)
	}
	return strings.Join(parts, "/")
}

// ---------------------------------------------------------------------------
// Store: opening and migrations
// ---------------------------------------------------------------------------

type Store struct {
	db *sql.DB
}

// migrations are applied in order, once each. SQLite's built-in
// "user_version" counter records how many have run, so on startup only the
// new ones execute. To change the schema later, APPEND a new entry — never
// edit an old one, since it has already run on your Pi.
var migrations = []string{
	// 1: initial schema
	`
	CREATE TABLE exercises (
		id    INTEGER PRIMARY KEY,
		name  TEXT NOT NULL UNIQUE,
		notes TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE presets (
		id   INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE
	);

	-- Which exercises a preset contains, in what order, and how many sets.
	CREATE TABLE preset_exercises (
		preset_id   INTEGER NOT NULL REFERENCES presets(id)   ON DELETE CASCADE,
		exercise_id INTEGER NOT NULL REFERENCES exercises(id) ON DELETE CASCADE,
		position    INTEGER NOT NULL,
		sets        INTEGER NOT NULL DEFAULT 3 CHECK (sets BETWEEN 1 AND 10),
		PRIMARY KEY (preset_id, exercise_id)
	);

	CREATE TABLE sessions (
		id           INTEGER PRIMARY KEY,
		preset_id    INTEGER NOT NULL REFERENCES presets(id),
		performed_at TEXT    NOT NULL,
		energy       INTEGER NOT NULL CHECK (energy BETWEEN 0 AND 10),
		warmup_done  INTEGER NOT NULL DEFAULT 0,
		notes        TEXT    NOT NULL DEFAULT '',
		created_at   TEXT    NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX sessions_performed_at ON sessions(performed_at);

	-- One row per exercise done in a session. No ON DELETE CASCADE on
	-- exercise_id: an exercise with logged history can't be deleted.
	CREATE TABLE session_exercises (
		id          INTEGER PRIMARY KEY,
		session_id  INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		exercise_id INTEGER NOT NULL REFERENCES exercises(id),
		position    INTEGER NOT NULL,
		weight      REAL,
		UNIQUE (session_id, exercise_id)
	);
	CREATE INDEX session_exercises_exercise ON session_exercises(exercise_id);

	-- One row per set: how many reps.
	CREATE TABLE session_sets (
		session_exercise_id INTEGER NOT NULL REFERENCES session_exercises(id) ON DELETE CASCADE,
		set_no              INTEGER NOT NULL,
		reps                INTEGER NOT NULL CHECK (reps >= 0),
		PRIMARY KEY (session_exercise_id, set_no)
	);
	`,

	// 2: allow deleting presets and exercises while keeping logged history.
	// The link becomes NULL (ON DELETE SET NULL) and the name is kept in a
	// *_name column. SQLite can't change a column's constraints in place,
	// so each table is rebuilt: create new → copy rows → drop old → rename.
	`
	CREATE TABLE sessions_new (
		id           INTEGER PRIMARY KEY,
		preset_id    INTEGER REFERENCES presets(id) ON DELETE SET NULL,
		preset_name  TEXT    NOT NULL DEFAULT '', -- filled in when the preset is deleted
		performed_at TEXT    NOT NULL,
		energy       INTEGER NOT NULL CHECK (energy BETWEEN 0 AND 10),
		warmup_done  INTEGER NOT NULL DEFAULT 0,
		notes        TEXT    NOT NULL DEFAULT '',
		created_at   TEXT    NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	INSERT INTO sessions_new (id, preset_id, performed_at, energy, warmup_done, notes, created_at)
		SELECT id, preset_id, performed_at, energy, warmup_done, notes, created_at FROM sessions;
	DROP TABLE sessions;
	ALTER TABLE sessions_new RENAME TO sessions;
	CREATE INDEX sessions_performed_at ON sessions(performed_at);

	CREATE TABLE session_exercises_new (
		id            INTEGER PRIMARY KEY,
		session_id    INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		exercise_id   INTEGER REFERENCES exercises(id) ON DELETE SET NULL,
		exercise_name TEXT    NOT NULL DEFAULT '', -- filled in when the exercise is deleted
		position      INTEGER NOT NULL,
		weight        REAL,
		UNIQUE (session_id, exercise_id)
	);
	INSERT INTO session_exercises_new (id, session_id, exercise_id, position, weight)
		SELECT id, session_id, exercise_id, position, weight FROM session_exercises;
	DROP TABLE session_exercises;
	ALTER TABLE session_exercises_new RENAME TO session_exercises;
	CREATE INDEX session_exercises_exercise ON session_exercises(exercise_id);
	`,
}

// OpenStore opens (creating if needed) the SQLite database at path, runs any
// pending migrations and seeds example data into an empty database.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	// Pragmas are applied to every connection the driver opens:
	//   foreign_keys  – enforce REFERENCES rules (off by default in SQLite)
	//   journal_mode  – WAL is more robust against power loss on an SD card
	//   busy_timeout  – wait instead of failing if the DB is briefly locked
	dsn := "file:" + path +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer at a time; a single connection keeps it simple.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.seed(); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	ctx := context.Background()

	// Use one dedicated connection so the PRAGMAs below apply to the
	// connection that runs the migrations.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	var version int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= len(migrations) {
		return nil
	}

	// The very first build had no migrations (user_version 0) but did create
	// tables. Its schema can't be upgraded automatically, so say so clearly.
	if version == 0 {
		var n int
		conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`).Scan(&n)
		if n > 0 {
			return errors.New("this database was created by an early test build and can't be upgraded; " +
				"delete it (the data/ folder) and restart")
		}
	}

	// Rebuilding a table means dropping it, and with foreign keys on, a DROP
	// would also cascade-delete the rows that point at it. So, following
	// SQLite's documented procedure, foreign keys are switched off while
	// migrating (this PRAGMA is ignored inside a transaction, so it goes
	// first) and checked by hand before each commit.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)

	for i := version; i < len(migrations); i++ {
		if err := runMigration(ctx, conn, i+1, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}

func runMigration(ctx context.Context, conn *sql.Conn, version int, script string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after a successful Commit

	if _, err := tx.ExecContext(ctx, script); err != nil {
		return err
	}

	// foreign_key_check returns one row per broken reference.
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	broken := rows.Next()
	rows.Close()
	if broken {
		return errors.New("foreign key check failed")
	}

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		return err
	}
	return tx.Commit()
}

// seed inserts placeholder exercises and two presets the first time the app
// runs, so there is something to click on. Rename them from the UI.
func (s *Store) seed() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM presets`).Scan(&n); err != nil || n > 0 {
		return err
	}
	ctx := context.Background()
	items := make([]PresetExercise, 6)
	for i := range items {
		id, err := s.CreateExercise(ctx, fmt.Sprintf("Exercise %d", i+1), "")
		if err != nil {
			return err
		}
		items[i] = PresetExercise{Exercise: Exercise{ID: id}, Sets: 3}
	}
	if _, err := s.SavePreset(ctx, 0, "Day A", items[0:3]); err != nil {
		return err
	}
	_, err := s.SavePreset(ctx, 0, "Day B", items[3:6])
	return err
}

// ---------------------------------------------------------------------------
// Exercises
// ---------------------------------------------------------------------------

func (s *Store) ListExercises(ctx context.Context) ([]Exercise, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, notes FROM exercises ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Exercise
	for rows.Next() {
		var e Exercise
		if err := rows.Scan(&e.ID, &e.Name, &e.Notes); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CreateExercise(ctx context.Context, name, notes string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO exercises (name, notes) VALUES (?, ?)`, name, notes)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ErrDuplicateName means another exercise already has that name, ignoring
// case ("Squat" vs "squat"). The UNIQUE constraint alone is case-sensitive.
var ErrDuplicateName = errors.New("an exercise with that name already exists")

// UpdateExercise renames an exercise. History follows automatically, because
// logged sets reference the exercise by id, not by name.
func (s *Store) UpdateExercise(ctx context.Context, id int64, name, notes string) error {
	var clash int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM exercises WHERE name = ? COLLATE NOCASE AND id <> ?`, name, id).Scan(&clash)
	if err != nil {
		return err
	}
	if clash > 0 {
		return ErrDuplicateName
	}
	_, err = s.db.ExecContext(ctx, `UPDATE exercises SET name = ?, notes = ? WHERE id = ?`, name, notes, id)
	return err
}

// AddExercises creates every name that doesn't exist yet (ignoring case, and
// ignoring repeats within names). It returns what was added and what was
// skipped as a duplicate.
func (s *Store) AddExercises(ctx context.Context, names []string) (added, skipped []string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	for _, name := range names {
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM exercises WHERE name = ? COLLATE NOCASE`, name).Scan(&exists); err != nil {
			return nil, nil, err
		}
		if exists > 0 {
			skipped = append(skipped, name)
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO exercises (name) VALUES (?)`, name); err != nil {
			return nil, nil, err
		}
		added = append(added, name)
	}
	return added, skipped, tx.Commit()
}

// ExerciseStat is an exercise plus where it's used, for the Exercises page.
type ExerciseStat struct {
	Exercise
	Presets     string // "Day A, Day B", or ""
	TimesLogged int
	LastLogged  time.Time // zero if never
}

func (s *Store) ListExerciseStats(ctx context.Context) ([]ExerciseStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.name, e.notes,
		       (SELECT group_concat(p.name, ', ')
		          FROM preset_exercises pe JOIN presets p ON p.id = pe.preset_id
		         WHERE pe.exercise_id = e.id),
		       (SELECT COUNT(*) FROM session_exercises se WHERE se.exercise_id = e.id),
		       (SELECT MAX(s.performed_at)
		          FROM session_exercises se JOIN sessions s ON s.id = se.session_id
		         WHERE se.exercise_id = e.id)
		FROM exercises e
		ORDER BY e.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ExerciseStat
	for rows.Next() {
		var (
			st            ExerciseStat
			presets, last sql.NullString
		)
		if err := rows.Scan(&st.ID, &st.Name, &st.Notes, &presets, &st.TimesLogged, &last); err != nil {
			return nil, err
		}
		st.Presets = presets.String
		if last.Valid {
			st.LastLogged, _ = time.ParseInLocation(dtLayout, last.String, time.Local)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// MergeExercise folds exercise "from" into "into" (for cleaning up a
// duplicate or typo): its logged sets and preset slots move over, then
// "from" is deleted. Nothing logged is lost:
//   - a session that already has "into" keeps both entries; the "from" one
//     is detached and kept under its old name, like a deleted exercise;
//   - a preset that already has "into" just drops the "from" slot.
func (s *Store) MergeExercise(ctx context.Context, from, into int64) error {
	if from == into {
		return errors.New("can't merge an exercise into itself")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Both exercises must exist.
	var found int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM exercises WHERE id IN (?, ?)`, from, into).Scan(&found); err != nil {
		return err
	}
	if found != 2 {
		return errors.New("one of those exercises no longer exists")
	}

	// ?1 = from, ?2 = into in every statement below.
	steps := []string{
		// Move logged entries; OR IGNORE skips sessions that already have "into".
		`UPDATE OR IGNORE session_exercises SET exercise_id = ?2 WHERE exercise_id = ?1`,
		// Whatever couldn't move: keep it, detached, under the old name.
		`UPDATE session_exercises SET exercise_name = (SELECT name FROM exercises WHERE id = ?1), exercise_id = NULL
		 WHERE exercise_id = ?1`,
		// Same for presets; leftovers are presets that already contain "into".
		`UPDATE OR IGNORE preset_exercises SET exercise_id = ?2 WHERE exercise_id = ?1`,
		`DELETE FROM preset_exercises WHERE exercise_id = ?1`,
		`DELETE FROM exercises WHERE id = ?1`,
	}
	for _, q := range steps {
		if _, err := tx.ExecContext(ctx, q, from, into); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteExercise removes an exercise and drops it from presets. Its logged
// sets are kept: the name is copied into the history first, then the
// delete clears the link (ON DELETE SET NULL).
func (s *Store) DeleteExercise(ctx context.Context, id int64) error {
	return s.deleteKeepingName(ctx, id,
		`UPDATE session_exercises SET exercise_name = (SELECT name FROM exercises WHERE id = ?1) WHERE exercise_id = ?1`,
		`DELETE FROM exercises WHERE id = ?1`)
}

// DeletePreset removes a preset. Sessions logged with it are kept, with its name.
func (s *Store) DeletePreset(ctx context.Context, id int64) error {
	return s.deleteKeepingName(ctx, id,
		`UPDATE sessions SET preset_name = (SELECT name FROM presets WHERE id = ?1) WHERE preset_id = ?1`,
		`DELETE FROM presets WHERE id = ?1`)
}

// deleteKeepingName runs the name-copying UPDATE and the DELETE in one
// transaction, so history never ends up without a name.
func (s *Store) deleteKeepingName(ctx context.Context, id int64, keepName, del string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, keepName, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, del, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Presets
// ---------------------------------------------------------------------------

// ListPresets returns every preset with its exercises in order.
func (s *Store) ListPresets(ctx context.Context) ([]Preset, error) {
	// A LEFT JOIN returns one row per (preset, exercise) pair, plus one row
	// with NULL exercise columns for presets that have no exercises yet.
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.name, e.id, e.name, e.notes, pe.sets
		FROM presets p
		LEFT JOIN preset_exercises pe ON pe.preset_id = p.id
		LEFT JOIN exercises e         ON e.id = pe.exercise_id
		ORDER BY p.name COLLATE NOCASE, p.id, pe.position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Preset
	for rows.Next() {
		var (
			pid           int64
			pname         string
			eid, sets     sql.NullInt64
			ename, enotes sql.NullString
		)
		if err := rows.Scan(&pid, &pname, &eid, &ename, &enotes, &sets); err != nil {
			return nil, err
		}
		// Rows arrive grouped by preset, so start a new one when the id changes.
		if len(out) == 0 || out[len(out)-1].ID != pid {
			out = append(out, Preset{ID: pid, Name: pname})
		}
		if eid.Valid {
			p := &out[len(out)-1]
			p.Exercises = append(p.Exercises, PresetExercise{
				Exercise: Exercise{ID: eid.Int64, Name: ename.String, Notes: enotes.String},
				Sets:     int(sets.Int64),
			})
		}
	}
	return out, rows.Err()
}

// GetPreset returns a single preset. With a handful of presets, filtering the
// full list is simpler than a second query and plenty fast.
func (s *Store) GetPreset(ctx context.Context, id int64) (Preset, error) {
	presets, err := s.ListPresets(ctx)
	if err != nil {
		return Preset{}, err
	}
	for _, p := range presets {
		if p.ID == id {
			return p, nil
		}
	}
	return Preset{}, sql.ErrNoRows
}

// SavePreset creates (id == 0) or updates a preset and replaces its exercise
// list with items, in the given order. Each item is either an existing
// exercise (Exercise.ID set) or a new one to create (ID 0, Exercise.Name
// set; reuses an exercise with that name if one exists). Everything runs in
// one transaction, so a failure never leaves a half-saved preset or stray
// new exercises.
func (s *Store) SavePreset(ctx context.Context, id int64, name string, items []PresetExercise) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // no-op after a successful Commit

	if id == 0 {
		res, err := tx.ExecContext(ctx, `INSERT INTO presets (name) VALUES (?)`, name)
		if err != nil {
			return 0, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE presets SET name = ? WHERE id = ?`, name, id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM preset_exercises WHERE preset_id = ?`, id); err != nil {
			return 0, err
		}
	}

	for pos, it := range items {
		exID := it.ID
		if exID == 0 {
			if exID, err = findOrCreateExercise(ctx, tx, it.Name); err != nil {
				return 0, err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO preset_exercises (preset_id, exercise_id, position, sets) VALUES (?, ?, ?, ?)`,
			id, exID, pos+1, it.Sets); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// findOrCreateExercise returns the id of the exercise called name (ignoring
// case), creating it if needed.
func findOrCreateExercise(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM exercises WHERE name = ? COLLATE NOCASE`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := tx.ExecContext(ctx, `INSERT INTO exercises (name) VALUES (?)`, name)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	return id, err
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// SessionFilter selects which sessions ListSessions returns. Zero values mean
// "don't filter on this".
type SessionFilter struct {
	ID       int64
	PresetID int64
	From, To string // "2006-01-02", both inclusive
	Limit    int    // newest N
}

func (f SessionFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ID != 0 {
		conds, args = append(conds, "s.id = ?"), append(args, f.ID)
	}
	if f.PresetID != 0 {
		conds, args = append(conds, "s.preset_id = ?"), append(args, f.PresetID)
	}
	// Timestamps are text like "2026-11-23T07:30", so plain string comparison
	// against a date works: everything on the 23rd is >= "2026-11-23" and
	// < "2026-11-24".
	if f.From != "" {
		conds, args = append(conds, "s.performed_at >= ?"), append(args, f.From)
	}
	if f.To != "" {
		conds, args = append(conds, "s.performed_at < date(?, '+1 day')"), append(args, f.To)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListSessions returns matching sessions newest first, each with its
// exercises and sets filled in.
func (s *Store) ListSessions(ctx context.Context, f SessionFilter) ([]Session, error) {
	where, args := f.where()
	// LEFT JOIN + COALESCE: use the preset's current name, or the name kept
	// in the session if the preset was deleted.
	q := `SELECT s.id, s.preset_id, COALESCE(p.name, s.preset_name),
	             s.performed_at, s.energy, s.warmup_done, s.notes
	      FROM sessions s LEFT JOIN presets p ON p.id = s.preset_id` + where +
		` ORDER BY s.performed_at DESC, s.id DESC`
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var (
			sess     Session
			presetID sql.NullInt64 // NULL once the preset is deleted
			when     string
		)
		if err := rows.Scan(&sess.ID, &presetID, &sess.PresetName, &when,
			&sess.Energy, &sess.WarmupDone, &sess.Notes); err != nil {
			return nil, err
		}
		sess.PresetID = presetID.Int64 // 0 when NULL
		if sess.PerformedAt, err = time.ParseInLocation(dtLayout, when, time.Local); err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	return out, s.loadExercises(ctx, out)
}

// loadExercises fills in Exercises for each session with a single query,
// rather than one query per session.
func (s *Store) loadExercises(ctx context.Context, sessions []Session) error {
	index := make(map[int64]int, len(sessions)) // session id -> slice index
	ids := make([]any, len(sessions))
	for i, sess := range sessions {
		index[sess.ID] = i
		ids[i] = sess.ID
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")

	rows, err := s.db.QueryContext(ctx, `
		SELECT se.id, se.session_id, se.exercise_id, COALESCE(e.name, se.exercise_name), se.weight, ss.reps
		FROM session_exercises se
		LEFT JOIN exercises e     ON e.id = se.exercise_id
		LEFT JOIN session_sets ss ON ss.session_exercise_id = se.id
		WHERE se.session_id IN (`+placeholders+`)
		ORDER BY se.session_id, se.position, se.id, ss.set_no`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			rowID, sid int64
			eid        sql.NullInt64 // NULL once the exercise is deleted
			name       string
			weight     sql.NullFloat64
			reps       sql.NullInt64
		)
		if err := rows.Scan(&rowID, &sid, &eid, &name, &weight, &reps); err != nil {
			return err
		}
		sess := &sessions[index[sid]]
		// One row per set; start a new exercise when the session_exercises row changes.
		if n := len(sess.Exercises); n == 0 || sess.Exercises[n-1].rowID != rowID {
			sess.Exercises = append(sess.Exercises,
				SessionExercise{ExerciseID: eid.Int64, Name: name, Weight: weight, rowID: rowID})
		}
		if reps.Valid {
			last := &sess.Exercises[len(sess.Exercises)-1]
			last.Reps = append(last.Reps, int(reps.Int64))
		}
	}
	return rows.Err()
}

func (s *Store) GetSession(ctx context.Context, id int64) (Session, error) {
	sessions, err := s.ListSessions(ctx, SessionFilter{ID: id})
	if err != nil {
		return Session{}, err
	}
	if len(sessions) == 0 {
		return Session{}, sql.ErrNoRows
	}
	return sessions[0], nil
}

// SaveSession inserts the session if sess.ID == 0, otherwise updates it.
// Its exercises and sets are replaced wholesale with sess.Exercises, except
// entries for deleted exercises, which can't be edited and are left as logged.
func (s *Store) SaveSession(ctx context.Context, sess Session) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	when := sess.PerformedAt.Format(dtLayout)
	if sess.ID == 0 {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (preset_id, performed_at, energy, warmup_done, notes)
			VALUES (?, ?, ?, ?, ?)`,
			sess.PresetID, when, sess.Energy, sess.WarmupDone, sess.Notes)
		if err != nil {
			return err
		}
		if sess.ID, err = res.LastInsertId(); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions SET performed_at = ?, energy = ?, warmup_done = ?, notes = ?
			WHERE id = ?`,
			when, sess.Energy, sess.WarmupDone, sess.Notes, sess.ID); err != nil {
			return err
		}
		// Sets go too, via ON DELETE CASCADE.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM session_exercises WHERE session_id = ? AND exercise_id IS NOT NULL`, sess.ID); err != nil {
			return err
		}
	}

	for pos, se := range sess.Exercises {
		if se.Deleted() {
			continue // kept as logged (see above)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO session_exercises (session_id, exercise_id, position, weight)
			VALUES (?, ?, ?, ?)`, sess.ID, se.ExerciseID, pos+1, se.Weight)
		if err != nil {
			return err
		}
		seID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		for i, reps := range se.Reps {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO session_sets (session_exercise_id, set_no, reps) VALUES (?, ?, ?)`,
				seID, i+1, reps); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteSession(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// PastPerformance is the most recent time an exercise was done.
type PastPerformance struct {
	SessionExercise
	PerformedAt time.Time
}

// LastPerformance returns the latest logged entry for an exercise, or nil if
// it has never been done. Used to pre-fill the log form.
func (s *Store) LastPerformance(ctx context.Context, exerciseID int64) (*PastPerformance, error) {
	var (
		seID int64
		when string
		p    = PastPerformance{SessionExercise: SessionExercise{ExerciseID: exerciseID}}
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT se.id, se.weight, s.performed_at
		FROM session_exercises se JOIN sessions s ON s.id = se.session_id
		WHERE se.exercise_id = ?
		ORDER BY s.performed_at DESC, s.id DESC
		LIMIT 1`, exerciseID).Scan(&seID, &p.Weight, &when)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if p.PerformedAt, err = time.ParseInLocation(dtLayout, when, time.Local); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT reps FROM session_sets WHERE session_exercise_id = ? ORDER BY set_no`, seID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r int
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		p.Reps = append(p.Reps, r)
	}
	return &p, rows.Err()
}

// SessionStats returns how many sessions exist and the first/last dates
// ("2006-01-02", empty if there are none), for the export page.
func (s *Store) SessionStats(ctx context.Context) (count int, first, last string, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(MIN(substr(performed_at, 1, 10)), ''),
		       COALESCE(MAX(substr(performed_at, 1, 10)), '')
		FROM sessions`).Scan(&count, &first, &last)
	return
}
