package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/triage"
)

const mainRepo = "argoproj/argo-workflows"

// openTest opens a fresh database in a temp directory, set up for the main
// repository.
func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "triage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	update(t, s, func(tx *Tx) error {
		set := DefaultSettings()
		set.Org, set.Repo = "argoproj", "argo-workflows"
		return tx.SaveSettings(context.Background(), set)
	})
	return s
}

func update(t *testing.T, s *Store, fn func(*Tx) error) {
	t.Helper()
	require.NoError(t, s.Update(context.Background(), fn))
}

// view runs fn in a read transaction and returns what it returns.
//
//nolint:ireturn // T is whatever fn returns; view only threads it through
func view[T any](t *testing.T, s *Store, fn func(context.Context, *Tx) (T, error)) T {
	t.Helper()
	var out T
	require.NoError(t, s.View(context.Background(), func(tx *Tx) error {
		var err error
		out, err = fn(context.Background(), tx)
		return err
	}))
	return out
}

// insert adds an issue to the main repository and returns it.
func insert(t *testing.T, s *Store, it Item) *Item {
	t.Helper()
	if it.Repo == "" {
		it.Repo = mainRepo
	}
	if it.Kind == "" {
		it.Kind = triage.KindIssue
	}
	update(t, s, func(tx *Tx) error {
		_, err := tx.InsertItem(context.Background(), &it)
		return err
	})
	return view(t, s, func(ctx context.Context, tx *Tx) (*Item, error) { return tx.ItemByID(ctx, it.ID) })
}

func at(day int) time.Time { return time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC) }

func TestOpenCreatesAPrivateWALDatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "triage.db")
	s, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// A write makes SQLite create the WAL and shared-memory files.
	update(t, s, func(tx *Tx) error { return tx.SaveSettings(context.Background(), DefaultSettings()) })

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, serr := os.Stat(p)
		require.NoError(t, serr, p)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "%s must be private: advisory notes live here", p)
	}
	dir, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dir.Mode().Perm())

	var mode string
	require.NoError(t, s.db.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode))
	assert.Equal(t, "wal", mode)

	var version int
	require.NoError(t, s.db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version))
	assert.Equal(t, SchemaVersion(), version)
}

// A database whose mode was loosened is made private again on open.
func TestOpenTightensALoosenedFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "triage.db")
	s, err := Open(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, os.Chmod(path, 0o644))

	s, err = Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// Reopening an up-to-date database must not re-run its migrations.
func TestOpenIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "triage.db")
	s, err := Open(context.Background(), path)
	require.NoError(t, err)
	update(t, s, func(tx *Tx) error {
		set := DefaultSettings()
		set.Org = "kept"
		return tx.SaveSettings(context.Background(), set)
	})
	require.NoError(t, s.Close())

	s, err = Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	got := view(t, s, func(ctx context.Context, tx *Tx) (Settings, error) { return tx.Settings(ctx) })
	assert.Equal(t, "kept", got.Org)
}

// A database from a newer build must be refused rather than written in a
// shape that build would misread.
func TestOpenRejectsNewerSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "triage.db")
	s, err := Open(context.Background(), path)
	require.NoError(t, err)
	_, err = s.db.ExecContext(context.Background(), "PRAGMA user_version = 99")
	require.NoError(t, err)
	require.NoError(t, s.Close())

	_, err = Open(context.Background(), path)
	require.ErrorContains(t, err, "schema version 99")
}

func TestSettings(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	got := view(t, s, func(ctx context.Context, tx *Tx) (Settings, error) { return tx.Settings(ctx) })
	assert.Equal(t, "argoproj/argo-workflows", got.MainRepo())
	assert.Equal(t, DefaultSettings().MaxOpenBeads, got.MaxOpenBeads)

	got.MaxOpenBeads, got.RetriageAfter, got.BeadLabel = 4, 36*time.Hour, "tb"
	update(t, s, func(tx *Tx) error { return tx.SaveSettings(context.Background(), got) })
	again := view(t, s, func(ctx context.Context, tx *Tx) (Settings, error) { return tx.Settings(ctx) })
	assert.Equal(t, got, again)
}

func TestSettingsSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key, value string
		want       string // when it reads back differently
		wantErr    string
	}{
		{key: KeyMaxOpenBeads, value: "12"},
		{key: KeyMaxOpenBeads, value: "0", wantErr: "positive whole number"},
		{key: KeyMaxTemplateAttempts, value: "x", wantErr: "positive whole number"},
		{key: KeyRetriageAfter, value: "720h", want: "720h0m0s"},
		{key: KeyRetriageAfter, value: "soon", wantErr: "positive duration"},
		{key: KeyBeadLabel, value: "", wantErr: "may not be empty"},
		{key: "colour", value: "blue", wantErr: "no such setting"},
	}
	for _, tc := range tests {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Parallel()
			s := DefaultSettings()
			err := s.Set(tc.key, tc.value)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			want := tc.want
			if want == "" {
				want = tc.value
			}
			assert.Equal(t, want, s.Get(tc.key))
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	assert.Nil(t, view(t, s, func(ctx context.Context, tx *Tx) (*time.Time, error) { return tx.Cursor(ctx) }))

	when := time.Date(2023, 4, 11, 9, 12, 0, 0, time.UTC)
	update(t, s, func(tx *Tx) error { return tx.SetCursor(context.Background(), &when) })
	got := view(t, s, func(ctx context.Context, tx *Tx) (*time.Time, error) { return tx.Cursor(ctx) })
	require.NotNil(t, got)
	assert.True(t, when.Equal(*got))
}

// Discovery enrols new items, refreshes known ones without disturbing their
// triage state, and brings in an item that a note added outside the pipeline.
func TestEnroll(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	triaged := insert(t, s, Item{Number: 1, Title: "old title", State: Triaged, UpdatedAt: at(1)})
	noted := insert(t, s, Item{Number: 2, Title: "noted"})

	results := map[int]bool{}
	update(t, s, func(tx *Tx) error {
		for _, n := range []int{1, 2, 3} {
			added, err := tx.Enroll(context.Background(), &Item{
				Repo: mainRepo, Kind: triage.KindIssue, Number: n, Title: "fresh title", UpdatedAt: at(5), GHState: "open",
			})
			if err != nil {
				return err
			}
			results[n] = added
		}
		return nil
	})

	assert.Equal(t, map[int]bool{1: false, 2: true, 3: true}, results)

	got := view(t, s, func(ctx context.Context, tx *Tx) (*Item, error) { return tx.ItemByID(ctx, triaged.ID) })
	assert.Equal(t, Triaged, got.State, "refreshing must not touch triage state")
	assert.Equal(t, "fresh title", got.Title)
	assert.True(t, at(5).Equal(got.UpdatedAt))

	got = view(t, s, func(ctx context.Context, tx *Tx) (*Item, error) { return tx.ItemByID(ctx, noted.ID) })
	assert.Equal(t, Untriaged, got.State, "an item outside the pipeline joins it when discovery reaches it")

	numbers := view(t, s, func(ctx context.Context, tx *Tx) (map[int]bool, error) {
		return tx.PipelineNumbers(ctx, mainRepo)
	})
	assert.Equal(t, map[int]bool{1: true, 2: true, 3: true}, numbers)
}

// Stored times must sort in the order they happened. RFC 3339 with a variable
// number of fractional digits does not ("...00Z" sorts after "...00.5Z"),
// which would quietly reorder the backlog.
func TestNextUntriagedOrdersByActivityAcrossFractionalSeconds(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	insert(t, s, Item{Number: 10, State: Untriaged, UpdatedAt: base.Add(500 * time.Millisecond)})
	insert(t, s, Item{Number: 20, State: Untriaged, UpdatedAt: base})
	insert(t, s, Item{Number: 30, State: Untriaged, UpdatedAt: base.Add(-time.Hour)})
	insert(t, s, Item{Number: 5, State: Untriaged, UpdatedAt: base}) // ties break by number
	insert(t, s, Item{Number: 40, State: Triaged, UpdatedAt: base.Add(-48 * time.Hour)})

	items := view(t, s, func(ctx context.Context, tx *Tx) ([]*Item, error) { return tx.NextUntriaged(ctx) })
	numbers := make([]int, 0, len(items))
	for _, it := range items {
		numbers = append(numbers, it.Number)
	}
	assert.Equal(t, []int{30, 5, 20, 10}, numbers)
}

func result(rec triage.Recommendation, reason triage.Reason) *triage.Result {
	return &triage.Result{Recommendation: rec, Reason: reason, Confidence: new(80), Reasoning: "because"}
}

// triage records a verdict on a queued item and returns the item.
func triageItem(t *testing.T, s *Store, number int, r *triage.Result, when time.Time) *Item {
	t.Helper()
	it := insert(t, s, Item{Number: number, State: Queued, BeadID: "tb-1"})
	update(t, s, func(tx *Tx) error { return tx.RecordVerdict(context.Background(), it.ID, "tb-1", r, when) })
	return view(t, s, func(ctx context.Context, tx *Tx) (*Item, error) { return tx.ItemByID(ctx, it.ID) })
}

func TestRecordVerdictIsCurrentAndPending(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	r := result(triage.Close, triage.Duplicate)
	r.DuplicateOf = new(100)
	r.SuggestedLabels = []string{"stale"}
	it := triageItem(t, s, 1, r, at(2))

	assert.Equal(t, Triaged, it.State)
	require.NotNil(t, it.Verdict)
	assert.Equal(t, *r, it.Verdict.Result)
	assert.Equal(t, "tb-1", it.Verdict.BeadID)
	assert.True(t, at(2).Equal(*it.Verdict.TriagedAt))
	assert.Equal(t, Pending, it.Verdict.Disposition.State)
}

// An empty list and an absent one are the same verdict, as they always were
// in the status file.
func TestEmptyListsReadBackAsNil(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	r := result(triage.Close, triage.Stale)
	r.SuggestedLabels, r.Evidence = []string{}, []string{}
	it := triageItem(t, s, 1, r, at(2))
	assert.Nil(t, it.Verdict.Result.SuggestedLabels)
	assert.Nil(t, it.Verdict.Result.Evidence)
}

func TestDispose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		disposition Disposition
		wantErr     error
		wantState   ItemState
		wantPending bool
	}{
		{name: "applied stays triaged", disposition: Disposition{State: Applied, Author: AuthorHuman},
			wantState: Triaged},
		{name: "deferred stays triaged", disposition: Disposition{State: Deferred, Author: AuthorClaude},
			wantState: Triaged},
		{name: "back to pending", disposition: Disposition{State: Pending, Author: AuthorHuman},
			wantState: Triaged, wantPending: true},
		{name: "rejected requeues", disposition: Disposition{State: Rejected, Note: "still repros", Author: AuthorHuman},
			wantState: Untriaged},
		{name: "rejected needs a note", disposition: Disposition{State: Rejected, Author: AuthorHuman},
			wantErr: ErrNoteRequired},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := openTest(t)
			it := triageItem(t, s, 1, result(triage.Close, triage.Stale), at(2))
			tc.disposition.At = new(at(3))

			err := s.Update(context.Background(), func(tx *Tx) error {
				return tx.Dispose(context.Background(), it, tc.disposition)
			})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)

			got := view(t, s, func(ctx context.Context, tx *Tx) (*Item, error) { return tx.ItemByID(ctx, it.ID) })
			assert.Equal(t, tc.wantState, got.State)
			pending := view(t, s, func(ctx context.Context, tx *Tx) ([]*Item, error) {
				return tx.Triaged(ctx, mainRepo, false)
			})
			assert.Equal(t, tc.wantPending, len(pending) == 1)

			history := view(t, s, func(ctx context.Context, tx *Tx) ([]Verdict, error) {
				return tx.VerdictHistory(ctx, it.ID)
			})
			require.Len(t, history, 1, "a decision never removes the verdict it was made on")
			assert.Equal(t, tc.disposition.State, history[0].Disposition.State)
			assert.Equal(t, tc.disposition.Author, history[0].Disposition.Author)
		})
	}
}

// Rejection is what puts the maintainer's reason in front of the next agent,
// and the rejected verdict stays readable as history once the item is
// requeued.
func TestRejectionCarriesTheNoteAndKeepsHistory(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	it := triageItem(t, s, 1, result(triage.Close, triage.Stale), at(2))
	update(t, s, func(tx *Tx) error {
		return tx.Dispose(context.Background(), it, Disposition{State: Rejected, Note: "still repros on 3.7", Author: AuthorHuman})
	})

	got := view(t, s, func(ctx context.Context, tx *Tx) (*Item, error) { return tx.ItemByID(ctx, it.ID) })
	assert.Equal(t, Untriaged, got.State)
	assert.Equal(t, "still repros on 3.7", got.CarryNote)
	assert.Nil(t, got.Verdict, "a requeued item has no verdict in force")
	assert.Empty(t, got.BeadID)

	history := view(t, s, func(ctx context.Context, tx *Tx) ([]Verdict, error) { return tx.VerdictHistory(ctx, it.ID) })
	require.Len(t, history, 1)
	assert.Equal(t, Rejected, history[0].Disposition.State)
	assert.Equal(t, "still repros on 3.7", history[0].Disposition.Note)

	// The next verdict becomes current, and clears the carried note.
	update(t, s, func(tx *Tx) error {
		if err := tx.MarkQueued(context.Background(), it.ID, "tb-2", at(4)); err != nil {
			return err
		}
		return tx.RecordVerdict(context.Background(), it.ID, "tb-2", result(triage.KeepOpen, triage.StillValid), at(5))
	})
	got = view(t, s, func(ctx context.Context, tx *Tx) (*Item, error) { return tx.ItemByID(ctx, it.ID) })
	require.NotNil(t, got.Verdict)
	assert.Equal(t, triage.KeepOpen, got.Verdict.Result.Recommendation)
	assert.Equal(t, Pending, got.Verdict.Disposition.State, "a new verdict starts pending")
	assert.Empty(t, got.CarryNote)
}

func TestDisposeRefusesAnItemWithoutAVerdict(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	it := insert(t, s, Item{Number: 1, State: Queued})
	err := s.Update(context.Background(), func(tx *Tx) error {
		return tx.Dispose(context.Background(), it, Disposition{State: Applied, Author: AuthorHuman})
	})
	require.ErrorIs(t, err, ErrNotTriaged)
	assert.Contains(t, err.Error(), "queued, not triaged")
}

func TestExpiredTriage(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	triageItem(t, s, 1, result(triage.Close, triage.Stale), at(1))
	window := 10 * 24 * time.Hour

	expired := func(now time.Time) int {
		return len(view(t, s, func(ctx context.Context, tx *Tx) ([]*Item, error) {
			return tx.ExpiredTriage(ctx, now, window)
		}))
	}
	assert.Equal(t, 0, expired(at(10)), "just short of the window")
	assert.Equal(t, 1, expired(at(11)), "exactly the window")
	assert.Equal(t, 1, expired(at(20)))
}

func TestReplaceResultKeepsTheDecision(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	it := triageItem(t, s, 1, result(triage.Close, triage.Stale), at(2))
	update(t, s, func(tx *Tx) error {
		return tx.Dispose(context.Background(), it, Disposition{State: Applied, Note: "done", Author: AuthorHuman})
	})

	fixed := result(triage.Close, triage.Stale)
	fixed.SuggestedComment = "the whole comment"
	update(t, s, func(tx *Tx) error { return tx.ReplaceResult(context.Background(), it.Verdict.ID, fixed) })

	got := view(t, s, func(ctx context.Context, tx *Tx) (*Item, error) { return tx.ItemByID(ctx, it.ID) })
	assert.Equal(t, "the whole comment", got.Verdict.Result.SuggestedComment)
	assert.Equal(t, Applied, got.Verdict.Disposition.State)
	assert.True(t, at(2).Equal(*got.Verdict.TriagedAt))
}

// The daemon and the CLI write the same file from different processes. Write
// transactions take the lock before they read, so concurrent read-modify-write
// cycles must all survive.
func TestConcurrentWritersFromSeparateHandlesLoseNothing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "triage.db")
	first, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })
	update(t, first, func(tx *Tx) error {
		set := DefaultSettings()
		set.Org, set.Repo = "argoproj", "argo-workflows"
		return tx.SaveSettings(context.Background(), set)
	})
	second, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })

	const writers = 20
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			handle := first
			if i%2 == 1 {
				handle = second
			}
			assert.NoError(t, handle.Update(context.Background(), func(tx *Tx) error {
				// Read-modify-write: a lost update would show as a short count.
				s, err := tx.Settings(context.Background())
				if err != nil {
					return err
				}
				s.MaxOpenBeads++
				return tx.SaveSettings(context.Background(), s)
			}))
		})
	}
	wg.Wait()

	got := view(t, first, func(ctx context.Context, tx *Tx) (Settings, error) { return tx.Settings(ctx) })
	assert.Equal(t, DefaultSettings().MaxOpenBeads+writers, got.MaxOpenBeads)
}

func TestInsertItemMarksAdvisoriesAndForksSensitive(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	issue := insert(t, s, Item{Number: 1})
	advisory := insert(t, s, Item{Kind: KindAdvisory, GHSA: "GHSA-aaaa-bbbb-cccc"})
	forkPR := insert(t, s, Item{Repo: "argoproj/argo-workflows-ghsa-aaaa-bbbb-cccc", Kind: triage.KindPR, Number: 1})

	assert.False(t, issue.Sensitive)
	assert.True(t, advisory.Sensitive)
	assert.True(t, forkPR.Sensitive)
}

func TestUpsertRefreshesWithoutBlanking(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	insert(t, s, Item{Number: 1, Title: "kept", URL: "https://example.com/1", State: Triaged})

	got := view(t, s, func(ctx context.Context, tx *Tx) (*Item, error) { return tx.Item(ctx, Ref{Repo: mainRepo, Number: 1}) })
	require.NoError(t, s.Update(context.Background(), func(tx *Tx) error {
		var err error
		got, err = tx.Upsert(context.Background(), &Item{Repo: mainRepo, Kind: triage.KindIssue, Number: 1, GHState: "closed"})
		return err
	}))
	assert.Equal(t, "kept", got.Title, "an empty title must not blank the stored one")
	assert.Equal(t, "closed", got.GHState)
	assert.Equal(t, Triaged, got.State)
}
