package app

import (
	"testing"

	"github.com/rborso/umkim-thin/internal/catalog"
)

// applyOIDCValues fills the generated client into an app's chart values. For an
// app with no dedicated case it used to *replace* the whole oidc map, which
// discarded every field the catalog had set there.
//
// The visible failure: Miniflux's chart reads oidc.redirectUrl to build
// OAUTH2_REDIRECT_URL. After the replace, the variable was empty, Miniflux sent
// an authorize request with no redirect_uri, and Authelia answered
//
//	400 invalid_request: The 'redirect_uri' parameter is required when using
//	OpenID Connect 1.0.
//
// so SSO could never complete even though the login page offered it.
func TestApplyOIDCValuesKeepsChartSpecificFields(t *testing.T) {
	in := New(Config{OIDCIssuer: "https://auth.example.com"})
	e := catalog.Entry{Name: "miniflux", Auth: catalog.Auth{Kind: "oidc"}}
	values := map[string]any{
		"oidc": map[string]any{
			"redirectUrl":  "https://miniflux.example.com/oauth2/oidc/callback",
			"providerName": "Directory",
			"userCreation": true,
		},
	}
	client := &autheliaClient{ClientID: "miniflux", ClientSecret: "s3cret"}

	in.applyOIDCValues(e, values, client)

	oidc, ok := values["oidc"].(map[string]any)
	if !ok {
		t.Fatalf("oidc values = %T, want a map", values["oidc"])
	}
	if got := oidc["redirectUrl"]; got != "https://miniflux.example.com/oauth2/oidc/callback" {
		t.Errorf("redirectUrl = %v; the catalog's value must survive (this is what Authelia requires)", got)
	}
	if got := oidc["providerName"]; got != "Directory" {
		t.Errorf("providerName = %v, want the catalog's Directory", got)
	}
	if got := oidc["userCreation"]; got != true {
		t.Errorf("userCreation = %v, want the catalog's true", got)
	}
	// The generated client must still win over anything the catalog had.
	if got := oidc["clientId"]; got != "miniflux" {
		t.Errorf("clientId = %v, want the generated one", got)
	}
	if got := oidc["clientSecret"]; got != "s3cret" {
		t.Errorf("clientSecret = %v, want the generated one", got)
	}
	if got := oidc["issuerUrl"]; got != "https://auth.example.com" {
		t.Errorf("issuerUrl = %v, want the configured issuer", got)
	}
	if got := oidc["enabled"]; got != true {
		t.Errorf("enabled = %v, want true", got)
	}
}

// An app whose catalog has no oidc block must still get a complete one.
func TestApplyOIDCValuesCreatesTheMapWhenAbsent(t *testing.T) {
	in := New(Config{OIDCIssuer: "https://auth.example.com"})
	e := catalog.Entry{Name: "newapp", Auth: catalog.Auth{Kind: "oidc"}}
	values := map[string]any{}
	in.applyOIDCValues(e, values, &autheliaClient{ClientID: "newapp", ClientSecret: "x"})
	oidc, ok := values["oidc"].(map[string]any)
	if !ok {
		t.Fatal("oidc values were not created")
	}
	for _, k := range []string{"enabled", "clientId", "clientSecret", "issuerUrl"} {
		if _, present := oidc[k]; !present {
			t.Errorf("oidc.%s missing", k)
		}
	}
}

// The AI stack nests the front end's OIDC under webui.oidc, so the generic oidc.*
// merge does not reach it. Leaving clientSecret empty is not a harmless default:
// Open WebUI only registers an OIDC provider when it has a secret (or PKCE), so
// the install came up with no login button at all - /api/config reported
// oauth.providers as {} and onboarding stayed true.
func TestApplyOIDCValuesReachesTheAIWebuiBlock(t *testing.T) {
	in := New(Config{OIDCIssuer: "https://auth.example.com"})
	e := catalog.Entry{Name: "ai", Auth: catalog.Auth{Kind: "oidc"}}
	values := map[string]any{
		"webui": map[string]any{
			"externalUrl": "https://chat.example.com",
			"oidc": map[string]any{
				"enabled":      true,
				"providerName": "Authelia",
			},
		},
	}
	client := &autheliaClient{ClientID: "openwebui", ClientSecret: "s3cret"}

	in.applyOIDCValues(e, values, client)

	webui, ok := values["webui"].(map[string]any)
	if !ok {
		t.Fatalf("webui values = %T, want a map", values["webui"])
	}
	if got := webui["externalUrl"]; got != "https://chat.example.com" {
		t.Errorf("externalUrl = %v; the catalog's value must survive", got)
	}
	oidc, ok := webui["oidc"].(map[string]any)
	if !ok {
		t.Fatalf("webui.oidc = %T, want a map", webui["oidc"])
	}
	if got := oidc["clientSecret"]; got != "s3cret" {
		t.Errorf("clientSecret = %v, want the generated one (an empty secret means no OIDC provider registers)", got)
	}
	if got := oidc["clientId"]; got != "openwebui" {
		t.Errorf("clientId = %v, want the generated one", got)
	}
	if got := oidc["issuerUrl"]; got != "https://auth.example.com" {
		t.Errorf("issuerUrl = %v, want the configured issuer", got)
	}
	if got := oidc["providerName"]; got != "Authelia" {
		t.Errorf("providerName = %v; the catalog's value must survive", got)
	}
}
