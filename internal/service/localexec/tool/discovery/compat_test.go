package discovery

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
)

// Discovery without any setup on the machine returns the desktop's empty
// result.
func TestReferenceDiscoveryMissing(t *testing.T) {
	ref := compattest.Load(t)
	var want map[string]any
	if err := json.Unmarshal(ref.DiscoveryMissing, &want); err != nil {
		t.Fatal(err)
	}
	source, _ := want["source"].(string)
	b, err := discoverySetup(t).Discover(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %s, want %s", b, ref.DiscoveryMissing)
	}
}

// The desktop's part of the redaction gives the desktop's titles. The
// oracle test compares the two on random texts; these are the cases that
// told them apart.
func TestReferenceDiscoveryTitles(t *testing.T) {
	ref := compattest.Load(t)
	if len(ref.DiscoveryTitles) == 0 || len(ref.CommandTitles) == 0 {
		t.Fatal("no titles in the reference data")
	}
	for _, c := range ref.DiscoveryTitles {
		if got := redactDesktop(c.Text); got != c.Whole {
			t.Errorf("redaction of %q\n got     %q\n desktop %q", c.Text, got, c.Whole)
		}
		if got := shorten(redactDesktop(c.Text)); got != c.Title {
			t.Errorf("title of %q\n got     %q\n desktop %q", c.Text, got, c.Title)
		}
		// What grokbot-tunnel redacts besides only takes away.
		if got := discoveryTitle(c.Text); got != c.Title && !strings.Contains(got, redacted) {
			t.Errorf("title of %q\n got     %q\n desktop %q", c.Text, got, c.Title)
		}
	}
	for _, c := range ref.CommandTitles {
		if got := joinTitle(c.Parts, redactDesktop); got != c.Title {
			t.Errorf("title of %q\n got     %q\n desktop %q", c.Parts, got, c.Title)
		}
	}
}
