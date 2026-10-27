// Package service is what the CLI and the MCP server both do with the store
// beyond running triage: show an item with everything known about it, search,
// add notes and links, record decisions, and list advisories. Keeping it in
// one place means a Claude session and the maintainer's terminal follow the
// same rules, differing only in the author they record.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/ingest"
	"github.com/Joibel/triage-bot/internal/store"
)

// Service works on one store for the repository its settings name.
type Service struct {
	Store  *store.Store
	GitHub github.Client
	// MainRepo is the triaged repository, "owner/name".
	MainRepo string
	// Now is injectable so tests can pin recorded times.
	Now func() time.Time
}

// New builds a service for the repository the store is set up for.
func New(ctx context.Context, st *store.Store, gh github.Client) (*Service, error) {
	var s store.Settings
	if err := st.View(ctx, func(tx *store.Tx) error {
		var err error
		s, err = tx.Settings(ctx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("failed to read settings: %w", err)
	}
	if s.Org == "" || s.Repo == "" {
		return nil, fmt.Errorf("%s does not name an org and repo: run `triage-bot init`", st.Path)
	}
	return &Service{Store: st, GitHub: gh, MainRepo: s.MainRepo()}, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// ParseRef reads a reference relative to the main repository.
func (s *Service) ParseRef(ref string) (store.Ref, error) {
	r, err := store.ParseRef(ref, s.MainRepo)
	if err != nil {
		return store.Ref{}, fmt.Errorf("bad reference: %w", err)
	}
	return r, nil
}

// Display renders a ref as a user would type it.
func (s *Service) Display(ref store.Ref) string { return ref.Display(s.MainRepo) }

// NotTrackedError says an item is not in the store.
type NotTrackedError struct{ Ref string }

func (e *NotTrackedError) Error() string { return e.Ref + " is not in the store" }

// lookup reads the item a ref names, or a NotTrackedError.
func (s *Service) lookup(ctx context.Context, tx *store.Tx, ref store.Ref) (*store.Item, error) {
	it, err := tx.Item(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &NotTrackedError{Ref: s.Display(ref)}
	}
	return it, err
}

// resolve returns the write that brings in the item a ref names: a no-op if
// the store holds it, otherwise one that records what GitHub says about it.
// Only the fetch is done here, outside any transaction, so nothing waits on
// the network with the write lock held.
func (s *Service) resolve(ctx context.Context, ref store.Ref) (ingest.Upsert, error) {
	var held bool
	if err := s.Store.View(ctx, func(tx *store.Tx) error {
		_, err := tx.Item(ctx, ref)
		switch {
		case err == nil:
			held = true
		case errors.Is(err, store.ErrNotFound):
		default:
			return err
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to look up %s: %w", s.Display(ref), err)
	}
	if held {
		return func(ctx context.Context, tx *store.Tx) (*store.Item, error) { return s.lookup(ctx, tx, ref) }, nil
	}
	upsert, err := ingest.Fetch(ctx, s.GitHub, s.MainRepo, ref)
	if err != nil {
		return nil, fmt.Errorf("%s is not in the store, and GitHub could not say what it is: %w", s.Display(ref), err)
	}
	return upsert, nil
}

// Detail is everything the store knows about one item.
type Detail struct {
	Item store.Item
	// Advisory is set when the item is one.
	Advisory *store.Advisory
	// History is every verdict, oldest first; the current one is last while
	// the item is triaged.
	History []store.Verdict
	Notes   []store.Note
	Links   []store.Link
}

// Show reads everything known about one item. It never fetches: an item the
// store does not hold is reported as such.
func (s *Service) Show(ctx context.Context, ref store.Ref) (*Detail, error) {
	var d Detail
	err := s.Store.View(ctx, func(tx *store.Tx) error {
		it, err := s.lookup(ctx, tx, ref)
		if err != nil {
			return err
		}
		d.Item = *it
		if it.Kind == store.KindAdvisory {
			if d.Advisory, err = tx.AdvisoryOf(ctx, it.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if d.History, err = tx.VerdictHistory(ctx, it.ID); err != nil {
			return err
		}
		if d.Notes, err = tx.Notes(ctx, it.ID); err != nil {
			return err
		}
		d.Links, err = tx.Links(ctx, it.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// AddNote records a note on an item, bringing the item in from GitHub first
// if the store does not hold it.
func (s *Service) AddNote(ctx context.Context, ref store.Ref, body string, tags []string,
	author store.Author) (*store.Item, *store.Note, error) {
	upsert, err := s.resolve(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	var (
		it   *store.Item
		note = &store.Note{Body: body, Tags: tags, Author: author, CreatedAt: s.now()}
	)
	err = s.Store.Update(ctx, func(tx *store.Tx) error {
		if it, err = upsert(ctx, tx); err != nil {
			return err
		}
		note.ItemID = it.ID
		return tx.AddNote(ctx, note)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to add a note to %s: %w", s.Display(ref), err)
	}
	return it, note, nil
}

// Linked is the outcome of Link: the two ends as stored, From first, and
// whether the link is new.
type Linked struct {
	From, To store.Item
	Type     store.LinkType
	Added    bool
}

// Link records "from <t> to", bringing either end in from GitHub first if
// the store does not hold it. advisory_for and fix_in_fork are turned round
// if need be so the advisory comes first.
func (s *Service) Link(ctx context.Context, from store.Ref, t store.LinkType, to store.Ref,
	author store.Author) (*Linked, error) {
	upsertFrom, err := s.resolve(ctx, from)
	if err != nil {
		return nil, err
	}
	upsertTo, err := s.resolve(ctx, to)
	if err != nil {
		return nil, err
	}
	out := &Linked{Type: t}
	err = s.Store.Update(ctx, func(tx *store.Tx) error {
		a, uerr := upsertFrom(ctx, tx)
		if uerr != nil {
			return uerr
		}
		b, uerr := upsertTo(ctx, tx)
		if uerr != nil {
			return uerr
		}
		if a, b, uerr = store.Orient(a, b, t); uerr != nil {
			return uerr
		}
		// AddLink marks b sensitive in place when the link makes it so.
		out.Added, uerr = tx.AddLink(ctx, a, b, t, author, s.now())
		out.From, out.To = *a, *b
		return uerr
	})
	if err != nil {
		return nil, fmt.Errorf("failed to link %s to %s: %w", s.Display(from), s.Display(to), err)
	}
	return out, nil
}

// Verdicts returns the main repository's triaged items, in number order: only
// those still awaiting a decision, unless all is set.
func (s *Service) Verdicts(ctx context.Context, all bool) ([]*store.Item, error) {
	var items []*store.Item
	if err := s.Store.View(ctx, func(tx *store.Tx) error {
		var err error
		items, err = tx.Triaged(ctx, s.MainRepo, all)
		return err
	}); err != nil {
		return nil, fmt.Errorf("failed to read verdicts: %w", err)
	}
	return items, nil
}

// Search finds notes and item titles.
func (s *Service) Search(ctx context.Context, q store.SearchQuery) ([]store.Hit, error) {
	var hits []store.Hit
	if err := s.Store.View(ctx, func(tx *store.Tx) error {
		var err error
		hits, err = tx.Search(ctx, q)
		return err
	}); err != nil {
		return nil, fmt.Errorf("failed to search: %w", err)
	}
	return hits, nil
}

// AdvisoryDetail is an advisory with every link touching it, its private
// fork's pull requests among them.
type AdvisoryDetail struct {
	store.Advisory
	Links []store.Link
}

// Advisories lists advisories, newest first: only open ones unless all.
func (s *Service) Advisories(ctx context.Context, all bool) ([]AdvisoryDetail, error) {
	var out []AdvisoryDetail
	if err := s.Store.View(ctx, func(tx *store.Tx) error {
		advisories, err := tx.Advisories(ctx, all)
		if err != nil {
			return err
		}
		for _, a := range advisories {
			links, err := tx.Links(ctx, a.ID)
			if err != nil {
				return err
			}
			out = append(out, AdvisoryDetail{Advisory: a, Links: links})
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to read advisories: %w", err)
	}
	return out, nil
}

// Decide records a decision on an item's current verdict. The item is read
// under the write lock, so a daemon requeue or another decision since it was
// last shown is refused rather than written over.
func (s *Service) Decide(ctx context.Context, ref store.Ref, state store.DispositionState, note string,
	author store.Author) (*store.Item, error) {
	var it *store.Item
	err := s.Store.Update(ctx, func(tx *store.Tx) error {
		var err error
		if it, err = s.lookup(ctx, tx, ref); err != nil {
			return err
		}
		at := s.now()
		if derr := tx.Dispose(ctx, it, store.Disposition{State: state, Note: note, At: &at, Author: author}); derr != nil {
			return fmt.Errorf("%s: %w", s.Display(ref), derr)
		}
		it, err = tx.ItemByID(ctx, it.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return it, nil
}
