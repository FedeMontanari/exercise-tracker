package main

import (
	"context"
	"database/sql"
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
