package discovery

import (
	"context"
	"encoding/json"
	"reflect"
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
