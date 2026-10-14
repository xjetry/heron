package store

import (
	"testing"
)

func TestThemeInstallationRetainsBothVersions(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	for _, version := range []string{"one", "two"} {
		_, err := s.PutTheme(t.Context(), Theme{ID: "a", Name: "A", Version: version, SDK: 1},
			[]ThemeFile{{Path: "index.html", Content: []byte(version)}}, []byte(version), false, 20)
		if err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListThemes(t.Context())
	if err != nil || len(list) != 2 {
		t.Fatalf("installed versions = %+v, %v; want both immutable versions", list, err)
	}
	for _, version := range list {
		if version.Enabled {
			t.Fatal("installation unexpectedly activated a version")
		}
	}
}
