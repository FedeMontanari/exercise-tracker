package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// newTestStore opens a fresh database in a temp dir that Go deletes after the test.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSessionRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	presets, err := s.ListPresets(ctx)
	if err != nil || len(presets) != 2 {
		t.Fatalf("want 2 seeded presets, got %d (err %v)", len(presets), err)
	}
	dayA := presets[0]
	ex1, ex2 := dayA.Exercises[0].ID, dayA.Exercises[1].ID

	at := func(s string) time.Time {
		tm, _ := time.ParseInLocation(dtLayout, s, time.Local)
		return tm
	}

	// Two sessions on different days.
	for _, sess := range []Session{
		{PresetID: dayA.ID, PerformedAt: at("2026-11-02T07:00"), Energy: 6, Exercises: []SessionExercise{
			{ExerciseID: ex1, Weight: sql.NullFloat64{Float64: 60, Valid: true}, Reps: []int{8, 8, 7}},
		}},
		{PresetID: dayA.ID, PerformedAt: at("2026-11-09T07:00"), Energy: 8, WarmupDone: true, Exercises: []SessionExercise{
			{ExerciseID: ex1, Weight: sql.NullFloat64{Float64: 62.5, Valid: true}, Reps: []int{8, 8, 8}},
			{ExerciseID: ex2, Reps: []int{12, 10}}, // bodyweight: no weight
		}},
	} {
		if err := s.SaveSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.ListSessions(ctx, SessionFilter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("want 2 sessions, got %d (err %v)", len(all), err)
	}
	newest := all[0]
	if got := newest.Exercises[0].RepsText(); got != "8/8/8" {
		t.Errorf("reps = %q, want 8/8/8", got)
	}
	if got := newest.Exercises[0].WeightText(); got != "62.5" {
		t.Errorf("weight = %q, want 62.5", got)
	}
	if newest.Exercises[1].Weight.Valid {
		t.Error("bodyweight exercise should have no weight")
	}

	// Date filter is inclusive on both ends.
	got, _ := s.ListSessions(ctx, SessionFilter{From: "2026-11-09", To: "2026-11-09"})
	if len(got) != 1 || got[0].Energy != 8 {
		t.Errorf("filter 9 Nov: got %d sessions", len(got))
	}

	// Last performance is the most recent one, used to pre-fill the form.
	last, err := s.LastPerformance(ctx, ex1)
	if err != nil || last == nil || !reflect.DeepEqual(last.Reps, []int{8, 8, 8}) {
		t.Errorf("last performance = %+v (err %v)", last, err)
	}

	// Renaming an exercise carries the history along.
	if err := s.UpdateExercise(ctx, ex1, "Squat", ""); err != nil {
		t.Fatal(err)
	}
	all, _ = s.ListSessions(ctx, SessionFilter{})
	if all[1].Exercises[0].Name != "Squat" {
		t.Errorf("renamed exercise shows as %q", all[1].Exercises[0].Name)
	}

	// Deleting an exercise and its preset keeps the logged history and names.
	if err := s.DeleteExercise(ctx, ex1); err != nil {
		t.Fatalf("delete exercise with history: %v", err)
	}
	if err := s.DeletePreset(ctx, dayA.ID); err != nil {
		t.Fatalf("delete preset with sessions: %v", err)
	}
	all, _ = s.ListSessions(ctx, SessionFilter{})
	if len(all) != 2 {
		t.Fatalf("want 2 sessions after deletes, got %d", len(all))
	}
	old := all[1]
	if old.PresetID != 0 || old.PresetName != "Day A" {
		t.Errorf("session preset = %d %q, want 0 \"Day A\"", old.PresetID, old.PresetName)
	}
	if se := old.Exercises[0]; !se.Deleted() || se.Name != "Squat" || se.RepsText() != "8/8/7" {
		t.Errorf("deleted exercise entry = %+v", se)
	}

	// Editing that session must leave the deleted exercise's sets alone.
	old.Energy = 9
	if err := s.SaveSession(ctx, old); err != nil {
		t.Fatal(err)
	}
	again, _ := s.GetSession(ctx, old.ID)
	if again.Energy != 9 || len(again.Exercises) != 1 || again.Exercises[0].RepsText() != "8/8/7" {
		t.Errorf("after edit: %+v", again)
	}
}

// TestMigrationKeepsData builds a database at schema version 1 with some
// data in it, then opens it normally so the later migrations run.
func TestMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		migrations[0],
		`PRAGMA user_version = 1`,
		`INSERT INTO exercises (id, name) VALUES (1, 'Squat')`,
		`INSERT INTO presets (id, name) VALUES (1, 'Day A')`,
		`INSERT INTO sessions (id, preset_id, performed_at, energy) VALUES (1, 1, '2026-11-02T07:00', 6)`,
		`INSERT INTO session_exercises (id, session_id, exercise_id, position, weight) VALUES (1, 1, 1, 1, 60)`,
		`INSERT INTO session_sets VALUES (1, 1, 8), (1, 2, 7)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sess, err := s.GetSession(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if sess.PresetName != "Day A" || len(sess.Exercises) != 1 ||
		sess.Exercises[0].Name != "Squat" || sess.Exercises[0].RepsText() != "8/7" {
		t.Errorf("after migration: %+v", sess)
	}
	// And the new delete behaviour works on the migrated tables.
	if err := s.DeleteExercise(context.Background(), 1); err != nil {
		t.Errorf("delete after migration: %v", err)
	}
}

// TestSavePresetCreatesExercises covers "+ Create" in the preset editor:
// new names are created (or matched to an existing one) in the same save.
func TestSavePresetCreatesExercises(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	exercises, _ := s.ListExercises(ctx)
	before := len(exercises)

	items := []PresetExercise{
		{Exercise: Exercise{Name: "Pull-up"}, Sets: 4},     // new
		{Exercise: Exercise{ID: exercises[0].ID}, Sets: 3}, // existing, by id
		{Exercise: Exercise{Name: "exercise 2"}, Sets: 2},  // existing, matched by name
	}
	id, err := s.SavePreset(ctx, 0, "Day C", items)
	if err != nil {
		t.Fatal(err)
	}

	p, err := s.GetPreset(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.ExerciseNames(); got != "Pull-up, Exercise 1, Exercise 2" {
		t.Errorf("preset order = %q", got)
	}
	if p.Exercises[0].Sets != 4 {
		t.Errorf("sets = %d, want 4", p.Exercises[0].Sets)
	}
	exercises, _ = s.ListExercises(ctx)
	if len(exercises) != before+1 {
		t.Errorf("want exactly one new exercise, got %d → %d", before, len(exercises))
	}

	// A failed save (duplicate preset name) must not leave a new exercise behind.
	_, err = s.SavePreset(ctx, 0, "Day C", []PresetExercise{{Exercise: Exercise{Name: "Dips"}, Sets: 3}})
	if err == nil {
		t.Fatal("expected duplicate preset name to fail")
	}
	exercises, _ = s.ListExercises(ctx)
	if len(exercises) != before+1 {
		t.Error("failed save left a stray exercise behind")
	}
}

func TestAddAndRenameRejectDuplicates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	added, skipped, err := s.AddExercises(ctx, []string{"Squat", "exercise 1", "squat", "Bench press"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, []string{"Squat", "Bench press"}) {
		t.Errorf("added = %v", added)
	}
	if !reflect.DeepEqual(skipped, []string{"exercise 1", "squat"}) {
		t.Errorf("skipped = %v", skipped)
	}

	exercises, _ := s.ListExercises(ctx)
	var benchID int64
	for _, e := range exercises {
		if e.Name == "Bench press" {
			benchID = e.ID
		}
	}
	if err := s.UpdateExercise(ctx, benchID, "SQUAT", ""); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("rename onto existing name: err = %v, want ErrDuplicateName", err)
	}
	if err := s.UpdateExercise(ctx, benchID, "bench Press", "flat"); err != nil {
		t.Errorf("changing only the case of its own name should work: %v", err)
	}
}

func TestMergeExercise(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	at := func(v string) time.Time { tm, _ := time.ParseInLocation(dtLayout, v, time.Local); return tm }

	s.AddExercises(ctx, []string{"Bench press", "Bnech press"})
	exercises, _ := s.ListExercises(ctx)
	ids := map[string]int64{}
	for _, e := range exercises {
		ids[e.Name] = e.ID
	}
	good, typo := ids["Bench press"], ids["Bnech press"]

	presets, _ := s.ListPresets(ctx)
	dayA := presets[0].ID
	// Day A has both; Day B only the typo.
	s.SavePreset(ctx, dayA, "Day A", []PresetExercise{{Exercise: Exercise{ID: good}, Sets: 3}, {Exercise: Exercise{ID: typo}, Sets: 3}})
	s.SavePreset(ctx, presets[1].ID, "Day B", []PresetExercise{{Exercise: Exercise{ID: typo}, Sets: 4}})

	// Session 1 logged only the typo; session 2 logged both.
	s.SaveSession(ctx, Session{PresetID: dayA, PerformedAt: at("2026-11-01T07:00"), Energy: 5,
		Exercises: []SessionExercise{{ExerciseID: typo, Reps: []int{8, 8}}}})
	s.SaveSession(ctx, Session{PresetID: dayA, PerformedAt: at("2026-11-02T07:00"), Energy: 5,
		Exercises: []SessionExercise{{ExerciseID: good, Reps: []int{10}}, {ExerciseID: typo, Reps: []int{6}}}})

	if err := s.MergeExercise(ctx, typo, good); err != nil {
		t.Fatal(err)
	}

	sessions, _ := s.ListSessions(ctx, SessionFilter{})
	byDay := map[string]Session{}
	for _, sess := range sessions {
		byDay[sess.PerformedAt.Format("02")] = sess
	}
	if se := byDay["01"].Exercises[0]; se.ExerciseID != good || se.RepsText() != "8/8" {
		t.Errorf("session 1 entry = %+v, want moved to Bench press", se)
	}
	// Session 2 already had Bench press: the typo entry is kept, detached.
	if got := byDay["02"].Exercises; len(got) != 2 || !got[1].Deleted() || got[1].Name != "Bnech press" {
		t.Errorf("session 2 entries = %+v", got)
	}

	presets, _ = s.ListPresets(ctx)
	if got := presets[0].ExerciseNames(); got != "Bench press" {
		t.Errorf("Day A = %q, want just Bench press", got)
	}
	if got := presets[1]; got.ExerciseNames() != "Bench press" || got.Exercises[0].Sets != 4 {
		t.Errorf("Day B = %+v, want Bench press with 4 sets", got)
	}
	if err := s.MergeExercise(ctx, typo, good); err == nil {
		t.Error("merging a deleted exercise should fail")
	}
}
