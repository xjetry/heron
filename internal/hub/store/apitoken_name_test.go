package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

func TestCreateAPITokenRejectsUncleanNames(t *testing.T) {
	t.Parallel()
	for _, tc := range uncleanNameCases {
		t.Run(tc.name, func(t *testing.T) {
			s, clk := open(t)
			_, err := s.CreateAPIToken(t.Context(), tc.raw, [32]byte{1}, clk.Now(), 100, nil, time.Time{})
			list, listErr := s.ListAPITokens(t.Context())
			if listErr != nil || len(list) != 0 {
				t.Errorf("rejected create left tokens = %+v, %v", list, listErr)
			}
			checkUncleanNameErr(t, tc, err)
		})
	}
}

func TestUpdateAPITokenRejectsUncleanNames(t *testing.T) {
	t.Parallel()
	for _, tc := range uncleanNameCases {
		t.Run(tc.name, func(t *testing.T) {
			s, clk := open(t)
			before, err := s.CreateAPIToken(t.Context(), "kept", [32]byte{1}, clk.Now(), 100, nil, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.UpdateAPIToken(t.Context(), before.ID, tc.raw, &TokenGrant{AllNodes: true, Permissions: []Permission{PermissionConfigure}}, clk.Now().Add(time.Hour), before.Version())
			after, _, readErr := s.APITokenByHash(t.Context(), [32]byte{1})
			if readErr != nil || !reflect.DeepEqual(after, before) {
				t.Errorf("rejected update changed token: before=%+v after=%+v err=%v", before, after, readErr)
			}
			checkUncleanNameErr(t, tc, err)
		})
	}
}

func TestAPITokenCanonicalNames(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	for i, name := range []string{"名", strings.Repeat("名", MaxNameRunes), "two words"} {
		tok, err := s.CreateAPIToken(t.Context(), name, [32]byte{byte(i + 1)}, clk.Now(), 100, nil, time.Time{})
		if err != nil || tok.Name != name {
			t.Fatalf("canonical create = %+v, %v; want %q", tok, err, name)
		}
		updated, err := s.UpdateAPIToken(t.Context(), tok.ID, name, nil, time.Time{}, tok.Version())
		if err != nil || updated.Name != name {
			t.Fatalf("canonical update = %+v, %v; want %q", updated, err, name)
		}
	}
}

func seedAPITokenNames(t *testing.T, db *sql.DB) {
	t.Helper()
	for i, name := range []string{strings.Repeat("名", 65), strings.Repeat("界", 63) + " x", " \t\n ", "kept", " \x01clean\u2066 "} {
		if _, err := db.Exec("INSERT INTO api_token (id,name,token_hash,created_at) VALUES (?,?,?,1)", i+1, name, hash(byte(i+1))); err != nil {
			t.Fatal(err)
		}
	}
}

func assertMigratedAPITokenNames(t *testing.T, s *Store) {
	t.Helper()
	tokens, err := s.ListAPITokens(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wants := []string{strings.Repeat("名", 64), strings.Repeat("界", 63), "token", "kept", "clean"}
	if len(tokens) != len(wants) {
		t.Fatalf("token count = %d, want %d", len(tokens), len(wants))
	}
	for i, tok := range tokens {
		if tok.Name != wants[i] {
			t.Errorf("token %d name = %q, want %q", tok.ID, tok.Name, wants[i])
		}
		if clean, err := CleanName(tok.Name); err != nil || clean != tok.Name {
			t.Errorf("token %d name is not canonical: CleanName = %q, %v", tok.ID, clean, err)
		}
	}
}

func TestMigrationNormalizesAPITokenNames(t *testing.T) {
	t.Parallel()
	t.Run("47", func(t *testing.T) {
		assertMigratedAPITokenNames(t, migrateFrom(t, 47, seedAPITokenNames))
	})
	t.Run("48", func(t *testing.T) {
		assertMigratedAPITokenNames(t, migrateFrom(t, 48, seedAPITokenNames))
	})
}

func TestRestoreOldSnapshotNormalizesAPITokenNames(t *testing.T) {
	t.Parallel()
	for _, version := range []int{47, 48} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path, db := frozenSchemaFixture(t, version)
			seedAPITokenNames(t, db)
			source := &Store{path: path, clk: clock.NewFake(time.Unix(2_000_000_000, 0))}
			config := filepath.Join(t.TempDir(), "config.db")
			snapshotConfigBefore49(t, source, config)
			target := filepath.Join(t.TempDir(), "target.db")
			if _, err := Restore(t.Context(), target, config, "", "", time.Now(), slog.Default()); err != nil {
				t.Fatal(err)
			}
			s, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			assertMigratedAPITokenNames(t, s)
		})
	}
}

func TestRestoreRejectsUncleanAPITokenNames(t *testing.T) {
	t.Parallel()
	for _, tc := range uncleanNameCases {
		t.Run(tc.name, func(t *testing.T) {
			source, clk := open(t)
			tok, err := source.CreateAPIToken(t.Context(), "valid", [32]byte{1}, clk.Now(), 100, nil, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, source, "UPDATE api_token SET name = ? WHERE id = ?", tc.raw, tok.ID)
			config := filepath.Join(t.TempDir(), "config.db")
			if err := source.SnapshotConfig(t.Context(), config); err != nil {
				t.Fatal(err)
			}
			seed, target := openAt(t)
			if _, err := seed.CreateAPIToken(t.Context(), "marker", [32]byte{7}, clk.Now(), 100, nil, time.Time{}); err != nil {
				t.Fatal(err)
			}
			seed.Close()
			before, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			_, restoreErr := Restore(t.Context(), target, config, "", "", time.Now(), slog.Default())
			after, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(before, after) {
				t.Errorf("rejected restore changed target database: %v", err)
			}
			want := fmt.Sprintf("config API token %d (%q): %s", tok.ID, tc.raw, tc.want)
			if restoreErr == nil || restoreErr.Error() != want {
				t.Fatalf("restore err=%v, want %q", restoreErr, want)
			}
		})
	}
}

func TestRestoreCanonicalAPITokenName(t *testing.T) {
	t.Parallel()
	source, clk := open(t)
	name := strings.Repeat("名", MaxNameRunes)
	if _, err := source.CreateAPIToken(t.Context(), name, [32]byte{1}, clk.Now(), 100, nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := source.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.db")
	if _, err := Restore(t.Context(), target, config, "", "", time.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tokens, err := s.ListAPITokens(t.Context())
	if err != nil || len(tokens) != 1 || tokens[0].Name != name {
		t.Fatalf("restored tokens = %+v, %v; want name %q", tokens, err, name)
	}
}
