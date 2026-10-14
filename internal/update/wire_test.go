package update

import (
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func TestStatusProtoCarriesSource(t *testing.T) {
	if got := StatusProto(Status{Supported: true, Source: "hub"}, "v1.0.0").GetSource(); got != "hub" {
		t.Fatalf("source = %q", got)
	}
}

func TestValidateStatusSource(t *testing.T) {
	for src, ok := range map[string]bool{"": true, "github": true, "hub": true, "ftp": false} {
		err := ValidateStatus(&heronv1.UpdateStatus{Source: src})
		if (err == nil) != ok {
			t.Errorf("%q: err %v", src, err)
		}
	}
}
