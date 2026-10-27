package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Setting keys, as stored and as `triage-bot config` names them.
const (
	KeyOrg                 = "org"
	KeyRepo                = "repo"
	KeyMaxOpenBeads        = "max_open_beads"
	KeyRetriageAfter       = "retriage_after"
	KeyMaxTemplateAttempts = "max_template_attempts"
	KeyBeadLabel           = "bead_label"
)

// SettingKeys lists every setting, in display order.
func SettingKeys() []string {
	return []string{KeyOrg, KeyRepo, KeyMaxOpenBeads, KeyRetriageAfter, KeyMaxTemplateAttempts, KeyBeadLabel}
}

// Settings reads the settings, with DefaultSettings for any not stored.
func (tx *Tx) Settings(ctx context.Context) (Settings, error) {
	pairs, err := queryAll(ctx, tx, "settings", func(row scanner) ([2]string, error) {
		var kv [2]string
		if err := row.Scan(&kv[0], &kv[1]); err != nil {
			return kv, fmt.Errorf("failed to read settings: %w", err)
		}
		return kv, nil
	}, "SELECT key, value FROM settings")
	if err != nil {
		return Settings{}, err
	}

	s := DefaultSettings()
	for _, kv := range pairs {
		if err := s.Set(kv[0], kv[1]); err != nil {
			return Settings{}, fmt.Errorf("stored setting is invalid: %w", err)
		}
	}
	return s, nil
}

// SaveSettings writes every setting.
func (tx *Tx) SaveSettings(ctx context.Context, s Settings) error {
	for _, key := range SettingKeys() {
		if _, err := tx.tx.ExecContext(ctx,
			"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value",
			key, s.Get(key)); err != nil {
			return fmt.Errorf("failed to save setting %s: %w", key, err)
		}
	}
	return nil
}

// Get renders one setting as text.
func (s Settings) Get(key string) string {
	switch key {
	case KeyOrg:
		return s.Org
	case KeyRepo:
		return s.Repo
	case KeyMaxOpenBeads:
		return strconv.Itoa(s.MaxOpenBeads)
	case KeyRetriageAfter:
		return s.RetriageAfter.String()
	case KeyMaxTemplateAttempts:
		return strconv.Itoa(s.MaxTemplateAttempts)
	case KeyBeadLabel:
		return s.BeadLabel
	default:
		return ""
	}
}

// Set parses one setting from text, refusing unknown keys and values that
// would stop the bot working.
func (s *Settings) Set(key, value string) error {
	switch key {
	case KeyOrg:
		s.Org = value
	case KeyRepo:
		s.Repo = value
	case KeyBeadLabel:
		if value == "" {
			return errors.New("bead_label may not be empty")
		}
		s.BeadLabel = value
	case KeyRetriageAfter:
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return fmt.Errorf("retriage_after must be a positive duration such as 4320h, not %q", value)
		}
		s.RetriageAfter = d
	case KeyMaxOpenBeads, KeyMaxTemplateAttempts:
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return fmt.Errorf("%s must be a positive whole number, not %q", key, value)
		}
		if key == KeyMaxOpenBeads {
			s.MaxOpenBeads = n
		} else {
			s.MaxTemplateAttempts = n
		}
	default:
		return fmt.Errorf("no such setting %q", key)
	}
	return nil
}

// Cursor reads how far discovery has walked the backlog, by last activity. Nil
// means it has not started.
func (tx *Tx) Cursor(ctx context.Context) (*time.Time, error) {
	var through sql.NullString
	if err := tx.tx.QueryRowContext(ctx, "SELECT updated_through FROM cursor WHERE id = 1").Scan(&through); err != nil {
		return nil, fmt.Errorf("failed to read the discovery cursor: %w", err)
	}
	return parseNullTime(through)
}

// SetCursor records how far discovery has walked.
func (tx *Tx) SetCursor(ctx context.Context, through *time.Time) error {
	if _, err := tx.tx.ExecContext(ctx, "UPDATE cursor SET updated_through = ? WHERE id = 1", nullTime(through)); err != nil {
		return fmt.Errorf("failed to save the discovery cursor: %w", err)
	}
	return nil
}

// timeLayout is fixed-width, always UTC with nine fractional digits, so stored
// times compare correctly as strings and keep their full precision.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// nullTime stores a nil or zero time as NULL.
func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

// zeroNull stores a zero time as NULL, the form an unset non-pointer time has.
func zeroNull(t time.Time) any { return nullTime(&t) }

func parseNullTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := time.Parse(timeLayout, s.String)
	if err != nil {
		return nil, fmt.Errorf("stored time %q is malformed: %w", s.String, err)
	}
	return &t, nil
}

// parseZeroTime reads a nullable time into a non-pointer, NULL being zero.
func parseZeroTime(s sql.NullString) (time.Time, error) {
	t, err := parseNullTime(s)
	if err != nil || t == nil {
		return time.Time{}, err
	}
	return *t, nil
}
