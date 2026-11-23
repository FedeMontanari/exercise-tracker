package main

import (
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// App bundles the dependencies every handler needs.
type App struct {
	store *Store
	pages map[string]*template.Template
	unit  string // weight unit label, "kg" or "lb"
}

// Page holds the fields shared by every page (used by layout.html).
// Page-specific data structs embed it, so templates can use .Title etc.
type Page struct {
	Title string
	Nav   string // which nav link to highlight
	Flash string // success message
	Error string // error message
}

// newPage builds a Page, picking up ?msg= / ?err= set by a previous redirect.
func newPage(r *http.Request, title, nav string) Page {
	q := r.URL.Query()
	return Page{Title: title, Nav: nav, Flash: q.Get("msg"), Error: q.Get("err")}
}

// NewApp parses every page template once at startup. Each page is combined
// with the shared layout and partials into its own template set, so the
// "content" block defined by each page doesn't clash with the others.
// weightUnit is the label shown next to weights ("kg" or "lb").
func NewApp(store *Store, weightUnit string) (*App, error) {
	funcs := template.FuncMap{
		"energyLevels": func() []int { return []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10} },
		"mul":          func(a, b int) int { return a * b },
		"add":          func(a, b int) int { return a + b },
		"unit":         func() string { return weightUnit },
	}

	pages := map[string]*template.Template{}
	for _, name := range []string{
		"index.html", "sessions.html", "session_edit.html", "progress.html",
		"exercises.html", "presets.html", "preset_form.html", "export.html",
	} {
		t, err := template.New(name).Funcs(funcs).ParseFS(assets,
			"templates/layout.html", "templates/partials.html", "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		pages[name] = t
	}
	return &App{store: store, pages: pages, unit: weightUnit}, nil
}

// Routes wires URLs to handlers using Go 1.22+ pattern routing
// ("METHOD /path/{param}"). HTML forms can only send GET and POST, so
// updates and deletes are POSTs to dedicated URLs.
func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.FileServerFS(assets))

	mux.HandleFunc("GET /{$}", a.index) // {$} = exactly "/", not a catch-all

	mux.HandleFunc("GET /sessions", a.listSessions)
	mux.HandleFunc("POST /sessions", a.createSession)
	mux.HandleFunc("GET /sessions/{id}/edit", a.editSession)
	mux.HandleFunc("POST /sessions/{id}", a.updateSession)
	mux.HandleFunc("POST /sessions/{id}/delete", a.deleteSession)

	mux.HandleFunc("GET /progress", a.progress)

	mux.HandleFunc("GET /exercises", a.listExercises)
	mux.HandleFunc("POST /exercises", a.createExercise)
	mux.HandleFunc("POST /exercises/{id}", a.updateExercise)
	mux.HandleFunc("POST /exercises/{id}/delete", a.deleteExercise)

	mux.HandleFunc("GET /presets", a.listPresets)
	mux.HandleFunc("GET /presets/new", a.presetForm)
	mux.HandleFunc("POST /presets", a.savePreset)
	mux.HandleFunc("GET /presets/{id}/edit", a.presetForm)
	mux.HandleFunc("POST /presets/{id}", a.savePreset)
	mux.HandleFunc("POST /presets/{id}/delete", a.deletePreset)

	mux.HandleFunc("GET /export", a.exportPage)
	mux.HandleFunc("GET /export/sessions.csv", a.exportSessions)

	return mux
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (a *App) render(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.pages[page].ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

// serverError logs the real error and shows the user a generic message.
func serverError(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	http.Error(w, "Something went wrong — check the server logs.", http.StatusInternalServerError)
}

// redirect sends the browser to path with an optional flash message
// (kind is "msg" for success or "err" for an error). This is the classic
// Post/Redirect/Get pattern: refreshing the page never re-submits a form.
func redirect(w http.ResponseWriter, r *http.Request, path, kind, text string) {
	if text != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + url.Values{kind: {text}}.Encode()
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// friendly turns common SQLite constraint errors into readable messages.
func friendly(err error, what string) string {
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Sprintf("That %s name is already taken.", what)
	}
	return err.Error()
}

func findPreset(presets []Preset, id int64) *Preset {
	for i := range presets {
		if presets[i].ID == id {
			return &presets[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Session form
// ---------------------------------------------------------------------------

// setCell is one reps box. Placeholder shows last time's reps in grey.
type setCell struct {
	Value, Placeholder string
}

// exerciseRow is one exercise line in the log form: weight + a box per set.
type exerciseRow struct {
	ExerciseID int64
	Name       string
	Weight     string
	Last       string // e.g. "Last (12 Nov): 60 × 8/8/7"
	Sets       []setCell

	// Deleted rows belong to an exercise that no longer exists; they are
	// shown read-only (Summary) and kept unchanged on save.
	Deleted bool
	Summary string
}

// presetBlock is one preset's exercise rows. The log form renders a block
// per preset and shows only the selected one, so switching preset doesn't
// reload the page or lose what you've typed.
type presetBlock struct {
	PresetID int64
	Name     string
	Rows     []exerciseRow
	Selected bool
}

// sessionFormData feeds the "sessionForm" partial.
type sessionFormData struct {
	Action      string
	Session     Session
	Blocks      []presetBlock // new session: one per preset; edit: just one
	Choosable   bool          // show the preset picker (new sessions only)
	SubmitLabel string
}

// summary formats a logged exercise as "60 kg × 8/8/7".
func summary(se SessionExercise, unit string) string {
	if w := se.WeightText(); w != "" {
		return w + " " + unit + " × " + se.RepsText()
	}
	return se.RepsText()
}

// rowsForNewSession builds the form rows for a preset, pre-filling each
// exercise's weight from last time and showing last time's reps as hints.
func (a *App) rowsForNewSession(r *http.Request, p Preset) ([]exerciseRow, error) {
	rows := make([]exerciseRow, 0, len(p.Exercises))
	for _, pe := range p.Exercises {
		row := exerciseRow{ExerciseID: pe.ID, Name: pe.Name, Sets: make([]setCell, pe.Sets)}

		last, err := a.store.LastPerformance(r.Context(), pe.ID)
		if err != nil {
			return nil, err
		}
		if last != nil {
			row.Weight = last.WeightText()
			for i := range row.Sets {
				if i < len(last.Reps) {
					row.Sets[i].Placeholder = strconv.Itoa(last.Reps[i])
				}
			}
			row.Last = fmt.Sprintf("Last (%s): %s", last.PerformedAt.Format("02 Jan"), summary(last.SessionExercise, a.unit))
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// rowsForEdit builds the form rows for an existing session: what was logged,
// followed by any of the preset's exercises that were skipped that day.
// p is empty if the preset has been deleted.
func (a *App) rowsForEdit(sess Session, p Preset) []exerciseRow {
	targetSets := map[int64]int{}
	for _, pe := range p.Exercises {
		targetSets[pe.ID] = pe.Sets
	}

	var rows []exerciseRow
	logged := map[int64]bool{}
	for _, se := range sess.Exercises {
		if se.Deleted() {
			rows = append(rows, exerciseRow{Name: se.Name, Deleted: true, Summary: summary(se, a.unit)})
			continue
		}
		logged[se.ExerciseID] = true
		cells := make([]setCell, max(len(se.Reps), targetSets[se.ExerciseID]))
		for i, reps := range se.Reps {
			cells[i].Value = strconv.Itoa(reps)
		}
		rows = append(rows, exerciseRow{ExerciseID: se.ExerciseID, Name: se.Name, Weight: se.WeightText(), Sets: cells})
	}
	for _, pe := range p.Exercises {
		if !logged[pe.ID] {
			rows = append(rows, exerciseRow{ExerciseID: pe.ID, Name: pe.Name, Sets: make([]setCell, pe.Sets)})
		}
	}
	return rows
}

// parseSessionForm reads the submitted form into sess. Each exercise row
// posts a hidden "exercise" id plus weight_<id> and one reps_<id> per set.
func parseSessionForm(r *http.Request, sess *Session) error {
	if err := r.ParseForm(); err != nil {
		return err
	}

	when, err := time.ParseInLocation(dtLayout, r.PostForm.Get("performed_at"), time.Local)
	if err != nil {
		return errors.New("invalid date/time")
	}
	sess.PerformedAt = when

	energy, err := strconv.Atoi(r.PostForm.Get("energy"))
	if err != nil || energy < 0 || energy > 10 {
		return errors.New("energy must be between 0 and 10")
	}
	sess.Energy = energy

	// An unchecked checkbox isn't sent at all, so presence == checked.
	sess.WarmupDone = r.PostForm.Has("warmup")
	sess.Notes = strings.TrimSpace(r.PostForm.Get("notes"))

	sess.Exercises = nil
	for _, v := range r.PostForm["exercise"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return errors.New("invalid exercise")
		}
		key := strconv.FormatInt(id, 10)

		se := SessionExercise{ExerciseID: id}
		for _, rv := range r.PostForm["reps_"+key] { // one value per set, in order
			if rv = strings.TrimSpace(rv); rv == "" {
				continue // empty box = set not done
			}
			n, err := strconv.Atoi(rv)
			if err != nil || n < 0 {
				return errors.New("reps must be whole numbers")
			}
			se.Reps = append(se.Reps, n)
		}
		if len(se.Reps) == 0 {
			continue // no reps entered = exercise skipped today
		}

		// Accept "62,5" as well as "62.5".
		if w := strings.ReplaceAll(strings.TrimSpace(r.PostForm.Get("weight_"+key)), ",", "."); w != "" {
			f, err := strconv.ParseFloat(w, 64)
			if err != nil || f < 0 {
				return errors.New("weight must be a number")
			}
			se.Weight = sql.NullFloat64{Float64: f, Valid: true}
		}
		sess.Exercises = append(sess.Exercises, se)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

type indexData struct {
	Page
	Form   *sessionFormData // nil when there are no presets yet
	Recent []Session
}

// index is the log page. ?preset=<id> pre-selects a workout; otherwise the
// one after last session's preset is selected.
func (a *App) index(w http.ResponseWriter, r *http.Request) {
	presets, err := a.store.ListPresets(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	recent, err := a.store.ListSessions(r.Context(), SessionFilter{Limit: 5})
	if err != nil {
		serverError(w, err)
		return
	}

	data := indexData{Page: newPage(r, "Log session", "log"), Recent: recent}

	selected := nextPreset(presets, recent)
	if id, err := strconv.ParseInt(r.URL.Query().Get("preset"), 10, 64); err == nil && findPreset(presets, id) != nil {
		selected = id
	}

	if len(presets) > 0 {
		form := &sessionFormData{
			Action:      "/sessions",
			Session:     Session{PerformedAt: time.Now(), Energy: 5},
			Choosable:   true,
			SubmitLabel: "Log session",
		}
		for _, p := range presets {
			rows, err := a.rowsForNewSession(r, p)
			if err != nil {
				serverError(w, err)
				return
			}
			form.Blocks = append(form.Blocks, presetBlock{
				PresetID: p.ID, Name: p.Name, Rows: rows, Selected: p.ID == selected,
			})
		}
		data.Form = form
	}
	a.render(w, "index.html", data)
}

// nextPreset picks the preset that follows the one used last time, so an
// A/B/C rotation needs zero taps to pick the right day.
func nextPreset(presets []Preset, recent []Session) int64 {
	if len(presets) == 0 {
		return 0
	}
	if len(recent) > 0 {
		for i, p := range presets {
			if p.ID == recent[0].PresetID {
				return presets[(i+1)%len(presets)].ID
			}
		}
	}
	return presets[0].ID
}

func (a *App) createSession(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirect(w, r, "/", "err", err.Error())
		return
	}
	presetID, err := strconv.ParseInt(r.PostForm.Get("preset_id"), 10, 64)
	if err == nil {
		_, err = a.store.GetPreset(r.Context(), presetID)
	}
	if err != nil {
		redirect(w, r, "/", "err", "Please pick a preset.")
		return
	}
	back := fmt.Sprintf("/?preset=%d", presetID)

	sess := Session{PresetID: presetID}
	if err := parseSessionForm(r, &sess); err != nil {
		redirect(w, r, back, "err", err.Error())
		return
	}
	if err := a.store.SaveSession(r.Context(), sess); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, "/", "msg", "Session logged 💪")
}

type sessionsData struct {
	Page
	Sessions []Session
}

func (a *App) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := a.store.ListSessions(r.Context(), SessionFilter{})
	if err != nil {
		serverError(w, err)
		return
	}
	a.render(w, "sessions.html", sessionsData{Page: newPage(r, "Sessions", "sessions"), Sessions: sessions})
}

type sessionEditData struct {
	Page
	Form sessionFormData
}

func (a *App) editSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sess, err := a.store.GetSession(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	// The preset may have been deleted since; then there's just the logged rows.
	preset, err := a.store.GetPreset(r.Context(), sess.PresetID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		serverError(w, err)
		return
	}
	a.render(w, "session_edit.html", sessionEditData{
		Page: newPage(r, "Edit session", "sessions"),
		Form: sessionFormData{
			Action:  fmt.Sprintf("/sessions/%d", id),
			Session: sess,
			Blocks: []presetBlock{{
				PresetID: sess.PresetID, Name: sess.PresetName, Rows: a.rowsForEdit(sess, preset), Selected: true,
			}},
			SubmitLabel: "Save changes",
		},
	})
}

func (a *App) updateSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sess, err := a.store.GetSession(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := parseSessionForm(r, &sess); err != nil {
		redirect(w, r, fmt.Sprintf("/sessions/%d/edit", id), "err", err.Error())
		return
	}
	if err := a.store.SaveSession(r.Context(), sess); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, "/sessions", "msg", "Session updated.")
}

func (a *App) deleteSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.store.DeleteSession(r.Context(), id); err != nil {
		serverError(w, err)
		return
	}
	// Go back to wherever the delete button was clicked (home or /sessions).
	back := "/sessions"
	if ref, err := url.Parse(r.Referer()); err == nil && ref.Path == "/" {
		back = "/"
	}
	redirect(w, r, back, "msg", "Session deleted.")
}

// ---------------------------------------------------------------------------
// Progress: the paper-style grid (rows = exercises, columns = sessions)
// ---------------------------------------------------------------------------

type progressCell struct {
	Weight, Reps string
	Up, Down     bool // weight changed vs the previous time this exercise was done
}

type progressRow struct {
	Name  string
	Cells []progressCell // one per session column
}

type progressData struct {
	Page
	Presets  []Preset
	Preset   *Preset
	N        int       // how many sessions to show; 0 = all
	Sessions []Session // oldest → newest, left to right
	Rows     []progressRow
}

func (a *App) progress(w http.ResponseWriter, r *http.Request) {
	presets, err := a.store.ListPresets(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	data := progressData{Page: newPage(r, "Progress", "progress"), Presets: presets, N: 8}
	if n, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && n >= 0 {
		data.N = n
	}

	if id, err := strconv.ParseInt(r.URL.Query().Get("preset"), 10, 64); err == nil {
		data.Preset = findPreset(presets, id)
	}
	if data.Preset == nil && len(presets) > 0 {
		data.Preset = &presets[0]
	}
	if data.Preset == nil {
		a.render(w, "progress.html", data)
		return
	}

	sessions, err := a.store.ListSessions(r.Context(), SessionFilter{PresetID: data.Preset.ID, Limit: data.N})
	if err != nil {
		serverError(w, err)
		return
	}
	// The store returns newest first; the grid reads left (old) to right (new).
	for i, j := 0, len(sessions)-1; i < j; i, j = i+1, j-1 {
		sessions[i], sessions[j] = sessions[j], sessions[i]
	}
	data.Sessions = sessions

	// Rows: the preset's exercises in order, then anything else that shows up
	// in these sessions (e.g. an exercise since removed from the preset).
	// Rows are keyed by exercise id, or by name for deleted exercises
	// (which no longer have an id).
	rowKey := func(id int64, name string) string {
		if id == 0 {
			return "deleted:" + name
		}
		return strconv.FormatInt(id, 10)
	}
	var order []string
	names := map[string]string{}
	addRow := func(id int64, name string) {
		if k := rowKey(id, name); names[k] == "" {
			order = append(order, k)
			names[k] = name
		}
	}
	for _, pe := range data.Preset.Exercises {
		addRow(pe.ID, pe.Name)
	}
	for _, s := range sessions {
		for _, se := range s.Exercises {
			addRow(se.ExerciseID, se.Name)
		}
	}

	for _, key := range order {
		row := progressRow{Name: names[key], Cells: make([]progressCell, len(sessions))}
		var prev sql.NullFloat64
		for i, s := range sessions {
			for _, se := range s.Exercises {
				if rowKey(se.ExerciseID, se.Name) != key {
					continue
				}
				cell := progressCell{Weight: se.WeightText(), Reps: se.RepsText()}
				if se.Weight.Valid && prev.Valid {
					cell.Up = se.Weight.Float64 > prev.Float64
					cell.Down = se.Weight.Float64 < prev.Float64
				}
				if se.Weight.Valid {
					prev = se.Weight
				}
				row.Cells[i] = cell
			}
		}
		data.Rows = append(data.Rows, row)
	}
	a.render(w, "progress.html", data)
}

// ---------------------------------------------------------------------------
// Exercises
// ---------------------------------------------------------------------------

type exercisesData struct {
	Page
	Exercises []Exercise
}

func (a *App) listExercises(w http.ResponseWriter, r *http.Request) {
	exercises, err := a.store.ListExercises(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	a.render(w, "exercises.html", exercisesData{Page: newPage(r, "Exercises", "exercises"), Exercises: exercises})
}

func exerciseFromForm(r *http.Request) (name, notes string, err error) {
	if err = r.ParseForm(); err != nil {
		return
	}
	name = strings.TrimSpace(r.PostForm.Get("name"))
	notes = strings.TrimSpace(r.PostForm.Get("notes"))
	if name == "" {
		err = errors.New("exercise name is required")
	}
	return
}

func (a *App) createExercise(w http.ResponseWriter, r *http.Request) {
	name, notes, err := exerciseFromForm(r)
	if err == nil {
		_, err = a.store.CreateExercise(r.Context(), name, notes)
	}
	if err != nil {
		redirect(w, r, "/exercises", "err", friendly(err, "exercise"))
		return
	}
	redirect(w, r, "/exercises", "msg", "Added “"+name+"”.")
}

func (a *App) updateExercise(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name, notes, err := exerciseFromForm(r)
	if err == nil {
		err = a.store.UpdateExercise(r.Context(), id, name, notes)
	}
	if err != nil {
		redirect(w, r, "/exercises", "err", friendly(err, "exercise"))
		return
	}
	redirect(w, r, "/exercises", "msg", "Saved “"+name+"”.")
}

func (a *App) deleteExercise(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.store.DeleteExercise(r.Context(), id); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, "/exercises", "msg", "Exercise deleted. Its logged sets are kept.")
}

// ---------------------------------------------------------------------------
// Presets
// ---------------------------------------------------------------------------

type presetsData struct {
	Page
	Presets []Preset
}

func (a *App) listPresets(w http.ResponseWriter, r *http.Request) {
	presets, err := a.store.ListPresets(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	a.render(w, "presets.html", presetsData{Page: newPage(r, "Presets", "presets"), Presets: presets})
}

// presetRow is one exercise line in the preset form: a checkbox, an order
// number and a target number of sets.
type presetRow struct {
	Exercise Exercise
	Checked  bool
	Position int
	Sets     int
}

type presetFormData struct {
	Page
	Preset Preset
	Rows   []presetRow
	Action string
}

// presetForm serves both /presets/new and /presets/{id}/edit.
func (a *App) presetForm(w http.ResponseWriter, r *http.Request) {
	var preset Preset
	if r.PathValue("id") != "" {
		id, err := pathID(r)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if preset, err = a.store.GetPreset(r.Context(), id); err != nil {
			http.NotFound(w, r)
			return
		}
	}

	exercises, err := a.store.ListExercises(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}

	// Mark exercises already in the preset, with their position and sets.
	inPreset := map[int64]presetRow{}
	for i, pe := range preset.Exercises {
		inPreset[pe.ID] = presetRow{Checked: true, Position: i + 1, Sets: pe.Sets}
	}
	rows := make([]presetRow, len(exercises))
	for i, e := range exercises {
		row, ok := inPreset[e.ID]
		if !ok {
			row.Sets = 3
		}
		row.Exercise = e
		rows[i] = row
	}
	// Show the preset's exercises first, in order, then the rest A–Z.
	sort.SliceStable(rows, func(i, j int) bool {
		pi, pj := rows[i].Position, rows[j].Position
		if pi == 0 || pj == 0 {
			return pi != 0 && pj == 0
		}
		return pi < pj
	})

	title, action := "New preset", "/presets"
	if preset.ID != 0 {
		title, action = "Edit "+preset.Name, fmt.Sprintf("/presets/%d", preset.ID)
	}
	a.render(w, "preset_form.html", presetFormData{
		Page: newPage(r, title, "presets"), Preset: preset, Rows: rows, Action: action,
	})
}

// savePreset handles both create (POST /presets) and update (POST /presets/{id}).
func (a *App) savePreset(w http.ResponseWriter, r *http.Request) {
	var id int64
	formURL := "/presets/new"
	if r.PathValue("id") != "" {
		var err error
		if id, err = pathID(r); err != nil {
			http.NotFound(w, r)
			return
		}
		formURL = fmt.Sprintf("/presets/%d/edit", id)
	}

	if err := r.ParseForm(); err != nil {
		redirect(w, r, formURL, "err", err.Error())
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" {
		redirect(w, r, formURL, "err", "Preset name is required.")
		return
	}

	// Collect ticked exercises and sort them by the order number typed next
	// to each one (blank orders go last, keeping their on-screen order).
	type pick struct {
		item     PresetExercise
		pos, idx int
	}
	var picks []pick
	for i, v := range r.PostForm["exercise"] {
		exID, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		pos, err := strconv.Atoi(r.PostForm.Get(fmt.Sprintf("pos_%d", exID)))
		if err != nil || pos <= 0 {
			pos = 1 << 30
		}
		sets, err := strconv.Atoi(r.PostForm.Get(fmt.Sprintf("sets_%d", exID)))
		if err != nil {
			sets = 3
		}
		sets = min(max(sets, 1), 10)
		picks = append(picks, pick{PresetExercise{Exercise: Exercise{ID: exID}, Sets: sets}, pos, i})
	}
	sort.Slice(picks, func(i, j int) bool {
		if picks[i].pos != picks[j].pos {
			return picks[i].pos < picks[j].pos
		}
		return picks[i].idx < picks[j].idx
	})
	items := make([]PresetExercise, len(picks))
	for i, p := range picks {
		items[i] = p.item
	}

	if _, err := a.store.SavePreset(r.Context(), id, name, items); err != nil {
		redirect(w, r, formURL, "err", friendly(err, "preset"))
		return
	}
	redirect(w, r, "/presets", "msg", "Saved “"+name+"”.")
}

func (a *App) deletePreset(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.store.DeletePreset(r.Context(), id); err != nil {
		serverError(w, err)
		return
	}
	redirect(w, r, "/presets", "msg", "Preset deleted. Sessions logged with it are kept.")
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

type exportRange struct {
	Label, From, To string
}

type exportData struct {
	Page
	Count       int
	First, Last string // dates of first/last session
	Today       string
	Ranges      []exportRange
}

func (a *App) exportPage(w http.ResponseWriter, r *http.Request) {
	count, first, last, err := a.store.SessionStats(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	const day = "2006-01-02"
	now := time.Now()
	today := now.Format(day)
	a.render(w, "export.html", exportData{
		Page:  newPage(r, "Export", "export"),
		Count: count, First: first, Last: last, Today: today,
		Ranges: []exportRange{
			{"Last 30 days", now.AddDate(0, 0, -29).Format(day), today},
			{"This month", now.Format("2006-01") + "-01", today},
			{"Last 3 months", now.AddDate(0, -3, 0).Format(day), today},
			{"This year", now.Format("2006") + "-01-01", today},
			{"All time", "", ""},
		},
	})
}

func boolInt(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// exportSessions streams all sessions between ?from= and ?to= (inclusive,
// either may be blank) as a "long" CSV: one row per set, with the session's
// details repeated on each row. That shape is the easiest to filter, pivot
// and chart in a spreadsheet or pandas.
func (a *App) exportSessions(w http.ResponseWriter, r *http.Request) {
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	for _, d := range []string{from, to} {
		if _, err := time.Parse("2006-01-02", d); d != "" && err != nil {
			http.Error(w, "dates must look like 2026-01-31", http.StatusBadRequest)
			return
		}
	}

	sessions, err := a.store.ListSessions(r.Context(), SessionFilter{From: from, To: to})
	if err != nil {
		serverError(w, err)
		return
	}

	filename := "sessions"
	if from != "" || to != "" {
		filename += "_" + orDefault(from, "start") + "_to_" + orDefault(to, time.Now().Format("2006-01-02"))
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`.csv"`)

	cw := csv.NewWriter(w)
	cw.Write([]string{"session_id", "date", "time", "weekday", "preset", "energy", "warmup_done", "notes",
		"exercise", "weight", "set", "reps"})

	// Oldest first reads more naturally in a spreadsheet.
	for i := len(sessions) - 1; i >= 0; i-- {
		s := sessions[i]
		base := []string{
			strconv.FormatInt(s.ID, 10),
			s.PerformedAt.Format("2006-01-02"),
			s.PerformedAt.Format("15:04"),
			s.PerformedAt.Weekday().String(),
			s.PresetName,
			strconv.Itoa(s.Energy),
			boolInt(s.WarmupDone),
			s.Notes,
		}
		if len(s.Exercises) == 0 {
			// Keep sessions with no sets logged, so energy/warm-up still count.
			cw.Write(append(base, "", "", "", ""))
			continue
		}
		for _, se := range s.Exercises {
			for setNo, reps := range se.Reps {
				// append to a copy so rows don't share the backing array
				row := append(append([]string{}, base...),
					se.Name, se.WeightText(), strconv.Itoa(setNo+1), strconv.Itoa(reps))
				cw.Write(row)
			}
		}
	}
	cw.Flush()
}

// orDefault returns s, or fallback when s is empty.
func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
