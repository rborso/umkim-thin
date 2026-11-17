package app

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rborso/umkim-thin/internal/catalog"
)

// Installing must not rewrite the live catalog snapshot. The store hands out
// shallow copies of Entry, so the nested Values maps are shared with it; a
// render that mutates them would serve generated secrets from GET /api/catalog
// and reuse the first install's rendering on the next one.
//
// This test loads the real catalog directory (not a fixture) so it also guards
// the shipped entries' shape, and compares the store's view before and after a
// full install-style render plus set() of a generated secret.
func TestInstallDoesNotMutateTheStoreSnapshot(t *testing.T) {
	store, err := catalog.NewStore("../../catalog", t.TempDir())
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}
	in := testInstaller()

	before, err := json.Marshal(store.Entries())
	if err != nil {
		t.Fatalf("snapshotting the catalog: %v", err)
	}
	if len(store.Entries()) == 0 {
		t.Fatal("the catalog is empty")
	}

	for name := range map[string]bool{"gitea": true, "vaultwarden": true, "immich": true} {
		e, ok := store.Entry(name)
		if !ok {
			continue
		}
		ctx, err := in.context(e, Options{Hosts: map[string]string{"": name + ".example.com"}})
		if err != nil {
			t.Fatalf("%s: context: %v", name, err)
		}
		values := in.renderValues(e, ctx)
		// The install path fills generated credentials into the rendered copy.
		set(values, "sso.clientSecret", "generated-"+name)
		set(values, "oidc.clientSecret", "generated-"+name)
		set(values, "postgres.password", "generated-"+name)
	}

	after, err := json.Marshal(store.Entries())
	if err != nil {
		t.Fatalf("re-snapshotting the catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the catalog snapshot changed during a render; generated values leak into the served catalog")
	}
}
