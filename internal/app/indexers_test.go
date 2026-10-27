package app

import "testing"

func TestSafeDefinitionFieldHidesPassword(t *testing.T) {
	field := safeDefinitionField(map[string]any{
		"name":  "password",
		"label": "Password",
		"type":  "password",
		"value": "must-not-leak",
	})
	if field.Value != nil {
		t.Fatalf("password value leaked: %#v", field.Value)
	}
	if field.Type != "password" {
		t.Fatalf("type: %q", field.Type)
	}
}

func TestNormalizeSubmittedField(t *testing.T) {
	def := ProwlarrDefinition{
		ProwlarrDefinitionSummary: ProwlarrDefinitionSummary{DefinitionName: "ncore"},
		Fields: []ProwlarrDefinitionField{
			{Name: "username", Type: "textbox"},
			{Name: "remember", Type: "checkbox"},
			{Name: "limit", Type: "number"},
			{Name: "baseUrl", Type: "select"},
			{Name: "password", Type: "password"},
		},
	}
	cases := []struct {
		name  string
		value any
		want  any
		ok    bool
	}{
		{"username", "someone", "someone", true},
		{"remember", true, true, true},
		{"limit", "25", "25", true},
		{"baseUrl", "https://tracker.example/", "https://tracker.example/", true},
		{"password", "secret value", "secret value", true},
		{"missing", "x", nil, false},
	}
	for _, tc := range cases {
		got, err := normalizeSubmittedField(def, tc.name, tc.value)
		if tc.ok != (err == nil) {
			t.Errorf("%s: err=%v", tc.name, err)
			continue
		}
		if tc.ok && got != tc.want {
			t.Errorf("%s: got %#v, want %#v", tc.name, got, tc.want)
		}
	}
}

func TestFilterDefinition(t *testing.T) {
	def := ProwlarrDefinitionSummary{
		DefinitionName: "ncore",
		Name:           "nCore",
		Description:    "Hungarian private tracker",
	}
	if !filterDefinition(def, "hungarian") || !filterDefinition(def, "NCORE") {
		t.Fatal("expected definition to match")
	}
	if filterDefinition(def, "not-a-tracker") {
		t.Fatal("unexpected match")
	}
}
