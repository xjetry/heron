package update

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestParseSourceConfig(t *testing.T) {
	for _, tc := range []struct{ body, want, err string }{
		{`{"source":"github"}`, "github", ""},
		{`{"source":"hub"}` + "\n", "hub", ""},
		{`{"source":"ftp"}`, "", "github or hub"},
		{`{}`, "", "github or hub"},
		{`{"source":"hub","extra":1}`, "", "unknown field"},
		{`{"source":"hub"} {"source":"github"}`, "", "single JSON object"},
		{``, "", "EOF"},
	} {
		got, err := parseSourceConfig([]byte(tc.body))
		if tc.err == "" && (err != nil || got != tc.want) {
			t.Errorf("%q: got %q err %v", tc.body, got, err)
		}
		if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("%q: err %v, want %q", tc.body, err, tc.err)
		}
	}
}

type namedSource string

func (namedSource) Fetch(context.Context, Request, string, string) (Artifacts, error) {
	return Artifacts{}, nil
}

func TestChooseSource(t *testing.T) {
	gh, hub := namedSource("github"), namedSource("hub")
	read := func(b string, err error) func() ([]byte, error) {
		return func() ([]byte, error) { return []byte(b), err }
	}
	if c := chooseSource("hub", func() ([]byte, error) { t.Fatal("hub role read the agent source config"); return nil, nil }, gh, hub); c.name != "github" || c.src != gh {
		t.Errorf("hub role: %+v", c)
	}
	if c := chooseSource("agent", read("", fs.ErrNotExist), gh, hub); c.name != "github" || c.src != gh || c.err != "" {
		t.Errorf("missing file: %+v", c)
	}
	if c := chooseSource("agent", read(`{"source":"hub"}`, nil), gh, hub); c.name != "hub" || c.src != hub || c.limit != hubFetchLimit {
		t.Errorf("hub file: %+v", c)
	}
	if c := chooseSource("agent", read(`{"source":"github"}`, nil), gh, hub); c.name != "github" || c.src != gh || c.limit != DownloadLimit {
		t.Errorf("github file: %+v", c)
	}
	for name, r := range map[string]func() ([]byte, error){
		"corrupt":    read(`{"source":`, nil),
		"permission": read("", fs.ErrPermission),
		"other":      read("", errors.New("managed file must be regular")),
	} {
		c := chooseSource("agent", r, gh, hub)
		if c.src != nil || !strings.Contains(c.err, sourceConfigPath) {
			t.Errorf("%s: %+v", name, c)
		}
	}
}
