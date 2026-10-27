package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The console's indexer manager is a thin, write-through Prowlarr client. It
// deliberately has no database and no console-side Secret: tracker credentials
// are sent to Prowlarr, which remains their only owner. This keeps the tracker
// choice optional and avoids coupling the platform to a specific tracker.
const prowlarrTimeout = 120 * time.Second

func prowlarrServiceURL(namespace string) string {
	if namespace == "" {
		namespace = ServarrNamespace
	}
	return fmt.Sprintf("http://prowlarr.%s.svc.cluster.local", namespace)
}

// ProwlarrIndexer is the non-secret summary the console shows for an existing
// indexer. Never add the indexer's fields here: Prowlarr's resource can include
// credential values.
type ProwlarrIndexer struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	Implementation string `json:"implementation"`
	Protocol       string `json:"protocol,omitempty"`
	Privacy        string `json:"privacy,omitempty"`
	Enable         bool   `json:"enable"`
}

// ProwlarrDefinitionSummary is a search result without its field values.
type ProwlarrDefinitionSummary struct {
	DefinitionName     string   `json:"definitionName"`
	Name               string   `json:"name"`
	Implementation     string   `json:"implementation"`
	ImplementationName string   `json:"implementationName"`
	Description        string   `json:"description,omitempty"`
	Language           string   `json:"language,omitempty"`
	Protocol           string   `json:"protocol,omitempty"`
	Privacy            string   `json:"privacy,omitempty"`
	IndexerURLs        []string `json:"indexerUrls,omitempty"`
}

// ProwlarrDefinitionField is the safe subset of Prowlarr's schema field. The
// browser needs enough metadata to render a form but never receives values for
// password fields.
type ProwlarrDefinitionField struct {
	Name     string           `json:"name"`
	Label    string           `json:"label,omitempty"`
	Type     string           `json:"type"`
	Value    any              `json:"value,omitempty"`
	HelpText string           `json:"helpText,omitempty"`
	Advanced bool             `json:"advanced,omitempty"`
	Privacy  string           `json:"privacy,omitempty"`
	Options  []map[string]any `json:"options,omitempty"`
	Provider string           `json:"selectOptionsProviderAction,omitempty"`
	IsFloat  bool             `json:"isFloat,omitempty"`
	Order    int              `json:"order,omitempty"`
}

// ProwlarrDefinition is one schema entry, with fields suitable for the form.
type ProwlarrDefinition struct {
	ProwlarrDefinitionSummary
	Fields []ProwlarrDefinitionField `json:"fields"`
}

// IndexerRequest is the only input the console accepts for an add. It contains
// credentials in transit to Prowlarr only; it is not persisted or logged.
type IndexerRequest struct {
	DefinitionName string         `json:"definitionName"`
	Name           string         `json:"name,omitempty"`
	Fields         map[string]any `json:"fields"`
	FlareSolverr   bool           `json:"flaresolverr,omitempty"`
}

func (in *Installer) prowlarrAPIKey(namespace string) (string, error) {
	if data, ok := in.getSecret(namespace, servarrBootstrapSecret); ok {
		if key := data["PROWLARR_API_KEY"]; key != "" {
			return key, nil
		}
	}
	if data, ok := in.getSecret(namespace, servarrAPIKeysSecret); ok {
		if key := data["prowlarr-api-key"]; key != "" {
			return key, nil
		}
	}
	return "", fmt.Errorf("Prowlarr API key is not available yet; run the media stack setup first")
}

func (in *Installer) prowlarrJSON(namespace, method, path string, body any) (int, []byte, error) {
	key, err := in.prowlarrAPIKey(namespace)
	if err != nil {
		return 0, nil, err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding Prowlarr request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, strings.TrimRight(prowlarrServiceURL(namespace), "/")+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: prowlarrTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("Prowlarr request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading Prowlarr response: %w", err)
	}
	return resp.StatusCode, data, nil
}

func decodeProwlarrArray(data []byte) ([]map[string]any, error) {
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("invalid Prowlarr response")
	}
	return rows, nil
}

func stringField(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return v
	}
	return ""
}

func boolField(row map[string]any, key string) bool {
	if v, ok := row[key].(bool); ok {
		return v
	}
	return false
}

func stringSlice(row map[string]any, key string) []string {
	raw, ok := row[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func intField(row map[string]any, key string) int {
	if v, ok := row[key].(float64); ok {
		return int(v)
	}
	return 0
}

func definitionSummary(row map[string]any) ProwlarrDefinitionSummary {
	return ProwlarrDefinitionSummary{
		DefinitionName:     stringField(row, "definitionName"),
		Name:               stringField(row, "name"),
		Implementation:     stringField(row, "implementation"),
		ImplementationName: stringField(row, "implementationName"),
		Description:        stringField(row, "description"),
		Language:           stringField(row, "language"),
		Protocol:           stringField(row, "protocol"),
		Privacy:            stringField(row, "privacy"),
		IndexerURLs:        stringSlice(row, "indexerUrls"),
	}
}

func safeDefinitionField(row map[string]any) ProwlarrDefinitionField {
	name := stringField(row, "name")
	field := ProwlarrDefinitionField{
		Name:     name,
		Label:    stringField(row, "label"),
		Type:     stringField(row, "type"),
		HelpText: stringField(row, "helpText"),
		Advanced: boolField(row, "advanced"),
		Privacy:  stringField(row, "privacy"),
		Provider: stringField(row, "selectOptionsProviderAction"),
		IsFloat:  boolField(row, "isFloat"),
		Order:    intField(row, "order"),
	}
	// Password values must never leave the server. Prowlarr normally sends null
	// here, but do not rely on that.
	if field.Type != "password" && field.Privacy != "password" {
		if v, ok := row["value"]; ok {
			field.Value = v
		}
	}
	if raw, ok := row["selectOptions"].([]any); ok {
		for _, item := range raw {
			if m, ok := item.(map[string]any); ok {
				field.Options = append(field.Options, m)
			}
		}
	}
	return field
}

func toProwlarrDefinition(row map[string]any) ProwlarrDefinition {
	def := ProwlarrDefinition{ProwlarrDefinitionSummary: definitionSummary(row)}
	if raw, ok := row["fields"].([]any); ok {
		for _, item := range raw {
			if m, ok := item.(map[string]any); ok {
				def.Fields = append(def.Fields, safeDefinitionField(m))
			}
		}
	}
	return def
}

func filterDefinition(def ProwlarrDefinitionSummary, q string) bool {
	if q == "" {
		return true
	}
	q = strings.ToLower(strings.TrimSpace(q))
	haystack := strings.ToLower(strings.Join([]string{
		def.DefinitionName, def.Name, def.Implementation, def.ImplementationName,
		def.Description, def.Language,
	}, " "))
	return strings.Contains(haystack, q)
}

// SearchProwlarrDefinitions returns summaries only; field values are fetched
// for the one definition the operator selects.
func (in *Installer) SearchProwlarrDefinitions(namespace, q string) ([]ProwlarrDefinitionSummary, error) {
	status, data, err := in.prowlarrJSON(namespace, http.MethodGet, "/api/v1/indexer/schema", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, prowlarrStatusError("read indexer definitions", status)
	}
	rows, err := decodeProwlarrArray(data)
	if err != nil {
		return nil, err
	}
	out := make([]ProwlarrDefinitionSummary, 0, len(rows))
	for _, row := range rows {
		summary := definitionSummary(row)
		if summary.DefinitionName == "" || !filterDefinition(summary, q) {
			continue
		}
		out = append(out, summary)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	if len(out) > 200 {
		out = out[:200]
	}
	return out, nil
}

// ProwlarrDefinition fetches one definition with form metadata. The definition
// name is matched against Prowlarr's own schema, so callers cannot inject a
// definition that Prowlarr does not ship.
func (in *Installer) ProwlarrDefinition(namespace, definitionName string) (ProwlarrDefinition, error) {
	status, data, err := in.prowlarrJSON(namespace, http.MethodGet, "/api/v1/indexer/schema", nil)
	if err != nil {
		return ProwlarrDefinition{}, err
	}
	if status != http.StatusOK {
		return ProwlarrDefinition{}, prowlarrStatusError("read indexer definitions", status)
	}
	rows, err := decodeProwlarrArray(data)
	if err != nil {
		return ProwlarrDefinition{}, err
	}
	for _, row := range rows {
		if stringField(row, "definitionName") == definitionName {
			return toProwlarrDefinition(row), nil
		}
	}
	return ProwlarrDefinition{}, fmt.Errorf("Prowlarr has no indexer definition %q", definitionName)
}

func (in *Installer) ListProwlarrIndexers(namespace string) ([]ProwlarrIndexer, error) {
	status, data, err := in.prowlarrJSON(namespace, http.MethodGet, "/api/v1/indexer", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, prowlarrStatusError("list indexers", status)
	}
	rows, err := decodeProwlarrArray(data)
	if err != nil {
		return nil, err
	}
	out := make([]ProwlarrIndexer, 0, len(rows))
	for _, row := range rows {
		out = append(out, ProwlarrIndexer{
			ID:             intField(row, "id"),
			Name:           stringField(row, "name"),
			Implementation: stringField(row, "implementation"),
			Protocol:       stringField(row, "protocol"),
			Privacy:        stringField(row, "privacy"),
			Enable:         boolField(row, "enable"),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (in *Installer) prowlarrAppProfileID(namespace string) int {
	status, data, err := in.prowlarrJSON(namespace, http.MethodGet, "/api/v1/appprofile", nil)
	if err != nil || status != http.StatusOK {
		return 1
	}
	rows, err := decodeProwlarrArray(data)
	if err != nil || len(rows) == 0 {
		return 1
	}
	if id := intField(rows[0], "id"); id > 0 {
		return id
	}
	return 1
}

func (in *Installer) ensureProwlarrFlareSolverrTag(namespace string) (int, error) {
	status, data, err := in.prowlarrJSON(namespace, http.MethodGet, "/api/v1/tag", nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, prowlarrStatusError("read tags", status)
	}
	rows, err := decodeProwlarrArray(data)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if stringField(row, "label") == "flaresolverr" {
			if id := intField(row, "id"); id > 0 {
				return id, nil
			}
		}
	}
	status, data, err = in.prowlarrJSON(namespace, http.MethodPost, "/api/v1/tag",
		map[string]any{"label": "flaresolverr"})
	if err != nil {
		return 0, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return 0, prowlarrStatusError("create flaresolverr tag", status)
	}
	var created map[string]any
	if json.Unmarshal(data, &created) != nil {
		return 0, fmt.Errorf("Prowlarr did not return the created tag")
	}
	if id := intField(created, "id"); id > 0 {
		return id, nil
	}
	return 0, fmt.Errorf("Prowlarr did not return the created tag id")
}

func prowlarrStatusError(action string, status int) error {
	return fmt.Errorf("Prowlarr %s failed (HTTP %d)", action, status)
}

// prowlarrValidationSummary extracts only property names from Prowlarr's
// validation response. It never returns body values, which could otherwise echo
// a submitted credential.
func prowlarrValidationSummary(data []byte) string {
	var rows []struct {
		PropertyName string `json:"propertyName"`
		ErrorMessage string `json:"errorMessage"`
	}
	if json.Unmarshal(data, &rows) != nil {
		return ""
	}
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		name := strings.TrimSpace(row.PropertyName)
		msg := strings.TrimSpace(row.ErrorMessage)
		if name == "" && msg == "" {
			continue
		}
		switch {
		case name != "" && msg != "":
			parts = append(parts, name+": "+msg)
		case name != "":
			parts = append(parts, name)
		default:
			parts = append(parts, msg)
		}
	}
	return strings.Join(parts, "; ")
}

func normalizeSubmittedField(def ProwlarrDefinition, name string, value any) (any, error) {
	for _, field := range def.Fields {
		if field.Name != name {
			continue
		}
		if field.Type == "password" {
			if value == nil {
				return "", nil
			}
			s, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("field %s must be a string", name)
			}
			return s, nil
		}
		switch field.Type {
		case "checkbox":
			if b, ok := value.(bool); ok {
				return b, nil
			}
			if s, ok := value.(string); ok {
				return s == "true" || s == "on" || s == "1", nil
			}
			return false, nil
		case "number":
			switch v := value.(type) {
			case float64:
				return v, nil
			case int:
				return v, nil
			case string:
				return v, nil
			default:
				return nil, fmt.Errorf("field %s must be a number", name)
			}
		case "select":
			if value == nil {
				return "", nil
			}
			switch v := value.(type) {
			case string, float64, bool:
				return v, nil
			default:
				return nil, fmt.Errorf("field %s has an invalid value", name)
			}
		default:
			if value == nil {
				return "", nil
			}
			s, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("field %s must be a string", name)
			}
			if len(s) > 8192 {
				return nil, fmt.Errorf("field %s is too long", name)
			}
			return s, nil
		}
	}
	return nil, fmt.Errorf("Prowlarr definition %q has no field %q", def.DefinitionName, name)
}

// AddProwlarrIndexer validates against the Prowlarr schema, tests the indexer,
// saves it, and triggers an app-indexer sync. The submitted credentials are
// used only in this call; they are neither written nor logged.
func (in *Installer) AddProwlarrIndexer(namespace string, req IndexerRequest) (ProwlarrIndexer, error) {
	req.DefinitionName = strings.TrimSpace(req.DefinitionName)
	if req.DefinitionName == "" {
		return ProwlarrIndexer{}, fmt.Errorf("an indexer definition is required")
	}
	def, err := in.ProwlarrDefinition(namespace, req.DefinitionName)
	if err != nil {
		return ProwlarrIndexer{}, err
	}

	// Re-fetch the raw schema entry because the sanitized definition above is
	// intentionally incomplete for rendering.
	status, data, err := in.prowlarrJSON(namespace, http.MethodGet, "/api/v1/indexer/schema", nil)
	if err != nil {
		return ProwlarrIndexer{}, err
	}
	if status != http.StatusOK {
		return ProwlarrIndexer{}, prowlarrStatusError("read indexer definitions", status)
	}
	rows, err := decodeProwlarrArray(data)
	if err != nil {
		return ProwlarrIndexer{}, err
	}
	var raw map[string]any
	for _, row := range rows {
		if stringField(row, "definitionName") == req.DefinitionName {
			raw = row
			break
		}
	}
	if raw == nil {
		return ProwlarrIndexer{}, fmt.Errorf("Prowlarr has no indexer definition %q", req.DefinitionName)
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = stringField(raw, "name")
	}
	if name == "" {
		name = req.DefinitionName
	}
	if len(name) > 120 {
		return ProwlarrIndexer{}, fmt.Errorf("indexer name is too long")
	}

	submitted := map[string]any{}
	for k, v := range req.Fields {
		if !allowedField(def, k) {
			return ProwlarrIndexer{}, fmt.Errorf("unknown Prowlarr field %q", k)
		}
		submitted[k] = v
	}
	// Ensure definitionFile cannot be changed by the caller.
	delete(submitted, "definitionFile")
	for _, field := range def.Fields {
		if field.Type == "info" {
			continue
		}
		if value, ok := submitted[field.Name]; ok {
			if _, err := normalizeSubmittedField(def, field.Name, value); err != nil {
				return ProwlarrIndexer{}, err
			}
		}
	}
	fields := make([]map[string]any, 0)
	rawFields, _ := raw["fields"].([]any)
	for _, item := range rawFields {
		field, ok := item.(map[string]any)
		if !ok {
			continue
		}
		nameKey := stringField(field, "name")
		if nameKey == "" {
			continue
		}
		out := map[string]any{}
		for k, v := range field {
			out[k] = v
		}
		if nameKey == "definitionFile" {
			out["value"] = req.DefinitionName
		} else if value, ok := submitted[nameKey]; ok {
			out["value"] = value
		} else if nameKey == "baseUrl" && out["value"] == nil {
			if urls := def.IndexerURLs; len(urls) > 0 {
				out["value"] = urls[0]
			}
		}
		fields = append(fields, out)
	}

	body := make(map[string]any, len(raw)+8)
	for k, v := range raw {
		body[k] = v
	}
	body["name"] = name
	body["enable"] = true
	body["appProfileId"] = in.prowlarrAppProfileID(namespace)
	body["priority"] = 25
	body["downloadClientId"] = 0
	body["fields"] = fields
	delete(body, "id")
	delete(body, "status")

	if req.FlareSolverr {
		tagID, err := in.ensureProwlarrFlareSolverrTag(namespace)
		if err != nil {
			return ProwlarrIndexer{}, err
		}
		body["tags"] = []int{tagID}
	} else {
		body["tags"] = []int{}
	}

	status, data, err = in.prowlarrJSON(namespace, http.MethodPost, "/api/v1/indexer/test", body)
	if err != nil {
		return ProwlarrIndexer{}, err
	}
	if status != http.StatusOK && status != http.StatusCreated && status != http.StatusNoContent {
		detail := prowlarrValidationSummary(data)
		if detail == "" {
			detail = "check the tracker credentials and required fields"
		}
		return ProwlarrIndexer{}, fmt.Errorf("Prowlarr indexer test failed: %s", detail)
	}

	status, data, err = in.prowlarrJSON(namespace, http.MethodPost, "/api/v1/indexer", body)
	if err != nil {
		return ProwlarrIndexer{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		detail := prowlarrValidationSummary(data)
		if detail == "" {
			detail = "Prowlarr rejected the indexer"
		}
		return ProwlarrIndexer{}, fmt.Errorf("Prowlarr could not add the indexer: %s", detail)
	}

	var created map[string]any
	if json.Unmarshal(data, &created) != nil {
		return ProwlarrIndexer{}, fmt.Errorf("Prowlarr added the indexer but returned an unreadable response")
	}
	// Sync is asynchronous and best-effort: the indexer is already saved.
	_, _, _ = in.prowlarrJSON(namespace, http.MethodPost, "/api/v1/command",
		map[string]any{"name": "ApplicationIndexerSync"})

	return ProwlarrIndexer{
		ID:             intField(created, "id"),
		Name:           stringField(created, "name"),
		Implementation: stringField(created, "implementation"),
		Protocol:       stringField(created, "protocol"),
		Privacy:        stringField(created, "privacy"),
		Enable:         boolField(created, "enable"),
	}, nil
}

func allowedField(def ProwlarrDefinition, name string) bool {
	for _, field := range def.Fields {
		if field.Name == name {
			return true
		}
	}
	return false
}
