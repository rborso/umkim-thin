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

// MediaProfile is the console's view of a Radarr/Sonarr quality profile. Only
// language variants of the configured target quality are managed here; the
// profile itself still owns the resolution/cutoff rules.
type MediaProfile struct {
	Name         string `json:"name"`
	Language     string `json:"language"`
	LanguageCode string `json:"languageCode"`
	Quality      string `json:"quality"`
	Default      bool   `json:"default"`
}

// MediaProfileRequest adds a language/resolution profile. It never changes the
// default profile.
type MediaProfileRequest struct {
	Language      string `json:"language"`
	QualityCutoff string `json:"qualityCutoff"`
}

const arrProfileTimeout = 60 * time.Second

func servarrLanguageName(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if i := strings.IndexAny(code, "-_"); i > 0 {
		code = code[:i]
	}
	for _, option := range servarrLanguageOptions {
		if option.Value == code {
			return option.Label
		}
	}
	return ""
}

func servarrLanguageCode(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, option := range servarrLanguageOptions {
		if strings.ToLower(option.Label) == name || option.Value == name {
			return option.Value
		}
	}
	return ""
}

func servarrQualityBase(quality string) string {
	quality = strings.ToLower(strings.TrimSpace(quality))
	if !strings.HasSuffix(quality, "p") {
		quality += "p"
	}
	switch quality {
	case "480p":
		return "SD-480p"
	case "2160p":
		return "UHD-2160p"
	default:
		return "HD-" + quality
	}
}

func profileQualityFromName(name, fallback string) string {
	prefix := name
	if i := strings.Index(prefix, " · "); i >= 0 {
		prefix = prefix[:i]
	}
	lower := strings.ToLower(prefix)
	switch {
	case strings.Contains(lower, "2160"), strings.Contains(lower, "uhd"), strings.Contains(lower, "4k"), strings.Contains(lower, "ultra"):
		return "2160p"
	case strings.Contains(lower, "1080"):
		return "1080p"
	case strings.Contains(lower, "720"):
		return "720p"
	case strings.Contains(lower, "480"), strings.Contains(lower, "sd"):
		return "480p"
	}
	return fallback
}

func baseProfileName(profiles []map[string]any, quality string) string {
	canonical := servarrQualityBase(quality)
	names := map[string]bool{}
	for _, profile := range profiles {
		names[mapString(profile, "name")] = true
	}
	if names[canonical] {
		return canonical
	}
	fallbacks := map[string][]string{
		"480p":  {"SD-480p", "SD"},
		"720p":  {"HD-720p"},
		"1080p": {"HD-1080p"},
		"2160p": {"UHD-2160p", "Ultra-HD"},
	}
	for _, name := range fallbacks[quality] {
		if names[name] {
			return name
		}
	}
	// Last resort: match the resolution in a profile name while avoiding the
	// built-in combined profiles where possible.
	for name := range names {
		if profileQualityFromName(name, "") == quality {
			return name
		}
	}
	return ""
}

func (in *Installer) servarrArrAPIKey(namespace, app string) (string, error) {
	if data, ok := in.getSecret(namespace, servarrAPIKeysSecret); ok {
		if key := data[app+"-api-key"]; key != "" {
			return key, nil
		}
	}
	if data, ok := in.getSecret(namespace, servarrBootstrapSecret); ok {
		envKey := strings.ToUpper(app) + "_API_KEY"
		if key := data[envKey]; key != "" {
			return key, nil
		}
	}
	return "", fmt.Errorf("%s API key is not available in namespace %s", app, namespace)
}

func (in *Installer) arrRequest(namespace, app, method, path string, body any) (int, []byte, error) {
	key, err := in.servarrArrAPIKey(namespace, app)
	if err != nil {
		return 0, nil, err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding %s request: %w", app, err)
		}
		reader = bytes.NewReader(raw)
	}
	url := fmt.Sprintf("http://%s.%s.svc.cluster.local%s", app, namespace, path)
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: arrProfileTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s request failed: %w", app, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading %s response: %w", app, err)
	}
	return resp.StatusCode, data, nil
}

func decodeObjectArray(data []byte) ([]map[string]any, error) {
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("invalid *arr response")
	}
	return rows, nil
}

func mapString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func mapInt(m map[string]any, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

func mapArray(m map[string]any, key string) []any {
	if v, ok := m[key].([]any); ok {
		return v
	}
	return nil
}

func (in *Installer) arrQualityProfiles(namespace, app string) ([]map[string]any, error) {
	status, data, err := in.arrRequest(namespace, app, http.MethodGet, "/api/v3/qualityprofile", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s profile listing failed (HTTP %d)", app, status)
	}
	return decodeObjectArray(data)
}

func (in *Installer) arrLanguageID(namespace, app, code string) (int, string, error) {
	want := strings.ToLower(servarrLanguageName(code))
	if want == "" {
		return 0, "", fmt.Errorf("unsupported language %q", code)
	}
	status, data, err := in.arrRequest(namespace, app, http.MethodGet, "/api/v3/language", nil)
	if err != nil {
		return 0, "", err
	}
	if status != http.StatusOK {
		return 0, "", fmt.Errorf("%s language listing failed (HTTP %d)", app, status)
	}
	rows, err := decodeObjectArray(data)
	if err != nil {
		return 0, "", err
	}
	for _, row := range rows {
		if strings.EqualFold(mapString(row, "name"), want) {
			return mapInt(row, "id"), want, nil
		}
	}
	return 0, "", fmt.Errorf("%s does not expose the %s language", app, want)
}

func (in *Installer) arrCustomFormats(namespace, app string) ([]map[string]any, error) {
	status, data, err := in.arrRequest(namespace, app, http.MethodGet, "/api/v3/customformat", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s custom format listing failed (HTTP %d)", app, status)
	}
	return decodeObjectArray(data)
}

// ensureArrProfile clones the base quality profile for one language. It never
// changes the base/default profile and is idempotent by profile name.
func (in *Installer) ensureArrProfile(namespace, app, quality, profileName, languageCode string) error {
	profiles, err := in.arrQualityProfiles(namespace, app)
	if err != nil {
		return err
	}
	var baseID int
	for _, profile := range profiles {
		if mapString(profile, "name") == profileName {
			return nil
		}
	}
	baseName := baseProfileName(profiles, quality)
	for _, profile := range profiles {
		if mapString(profile, "name") == baseName {
			baseID = mapInt(profile, "id")
		}
	}
	if baseID == 0 {
		return fmt.Errorf("%s has no %s quality profile to clone", app, baseName)
	}
	status, data, err := in.arrRequest(namespace, app, http.MethodGet,
		fmt.Sprintf("/api/v3/qualityprofile/%d", baseID), nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%s profile read failed (HTTP %d)", app, status)
	}
	var profile map[string]any
	if json.Unmarshal(data, &profile) != nil {
		return fmt.Errorf("%s returned an unreadable quality profile", app)
	}
	delete(profile, "id")
	profile["name"] = profileName

	if app == "radarr" {
		id, name, err := in.arrLanguageID(namespace, app, languageCode)
		if err != nil {
			return err
		}
		profile["language"] = map[string]any{"id": id, "name": name}
	}

	// The *arr profile validator requires every custom format to be present.
	// Clone the base scores, then make the Hungarian format decisive only for
	// Hungarian profiles.
	formats, err := in.arrCustomFormats(namespace, app)
	if err != nil {
		return err
	}
	current := map[int]map[string]any{}
	for _, item := range mapArray(profile, "formatItems") {
		if m, ok := item.(map[string]any); ok {
			current[mapInt(m, "format")] = m
		}
	}
	formatItems := make([]map[string]any, 0, len(formats))
	for _, format := range formats {
		id := mapInt(format, "id")
		item := current[id]
		if item == nil {
			item = map[string]any{"format": id, "name": mapString(format, "name")}
		}
		score := mapInt(item, "score")
		if strings.EqualFold(mapString(format, "name"), "Hungarian (HuN)") {
			if languageCode == "hu" {
				score = 10000
			} else {
				score = 0
			}
		}
		formatItems = append(formatItems, map[string]any{
			"format": id,
			"name":   mapString(format, "name"),
			"score":  score,
		})
	}
	profile["formatItems"] = formatItems

	status, data, err = in.arrRequest(namespace, app, http.MethodPost, "/api/v3/qualityprofile", profile)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("%s could not create profile %s (HTTP %d)", app, profileName, status)
	}
	_ = data
	return nil
}

// EnsureServarrProfiles creates language variants of the configured target
// quality for one installed release. The base/default profile is untouched.
func (in *Installer) EnsureServarrProfiles(namespace, release, quality, defaultLanguage string, languages []string, progress func(string)) error {
	settings, err := in.ServarrSettingsFor(release, namespace)
	if err != nil {
		return err
	}
	if quality == "" {
		quality = settings.QualityCutoff
	}
	defaultCode := strings.ToLower(strings.TrimSpace(defaultLanguage))
	if defaultCode == "" {
		defaultCode = settings.MediaLanguage
	}
	seen := map[string]bool{defaultCode: true}
	for _, raw := range languages {
		code := strings.ToLower(strings.TrimSpace(raw))
		if i := strings.IndexAny(code, "-_"); i > 0 {
			code = code[:i]
		}
		if code == "" || seen[code] {
			continue
		}
		name := servarrLanguageName(code)
		if name == "" {
			return fmt.Errorf("unsupported language %q", raw)
		}
		seen[code] = true
		profileName := servarrQualityBase(quality) + " · " + name
		if progress != nil {
			progress("adding " + profileName + " profile")
		}
		if err := in.ensureArrProfile(namespace, "radarr", quality, profileName, code); err != nil {
			return err
		}
		if err := in.ensureArrProfile(namespace, "sonarr", quality, profileName, code); err != nil {
			return err
		}
	}
	if len(seen) > 1 {
		if err := in.flushSeerrCaches(namespace); err != nil {
			if progress != nil {
				progress("WARN: media profiles created, but Seerr cache refresh failed: " + err.Error())
			}
		}
	}
	return nil
}

// ListServarrProfiles returns the language variants belonging to one installed
// release's target quality. Radarr supplies the language metadata; the profile
// set is mirrored in Sonarr by EnsureServarrProfiles.
func (in *Installer) ListServarrProfiles(namespace, release string) ([]MediaProfile, error) {
	settings, err := in.ServarrSettingsFor(release, namespace)
	if err != nil {
		return nil, err
	}
	base := servarrQualityBase(settings.QualityCutoff)
	profiles, err := in.arrQualityProfiles(namespace, "radarr")
	if err != nil {
		return nil, err
	}
	out := make([]MediaProfile, 0)
	for _, profile := range profiles {
		name := mapString(profile, "name")
		if name != base && !strings.Contains(name, " · ") {
			continue
		}
		language := ""
		if lang, ok := profile["language"].(map[string]any); ok {
			language = mapString(lang, "name")
		}
		out = append(out, MediaProfile{
			Name:         name,
			Language:     language,
			LanguageCode: servarrLanguageCode(language),
			Quality:      profileQualityFromName(name, settings.QualityCutoff),
			Default:      name == base,
		})
	}
	qualityRank := map[string]int{"480p": 1, "720p": 2, "1080p": 3, "2160p": 4}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		if out[i].Quality != out[j].Quality {
			return qualityRank[out[i].Quality] < qualityRank[out[j].Quality]
		}
		return strings.ToLower(out[i].Language) < strings.ToLower(out[j].Language)
	})
	return out, nil
}

func (in *Installer) seerrSession(namespace string) (string, []*http.Cookie, error) {
	data, ok := in.getSecret(namespace, servarrBootstrapSecret)
	if !ok {
		return "", nil, fmt.Errorf("servarr bootstrap secret missing in %s", namespace)
	}
	user := data["JELLYFIN_ADMIN_USER"]
	password := data["JELLYFIN_ADMIN_PASSWORD"]
	if user == "" {
		user = "jellyfin-admin"
	}
	if password == "" {
		return "", nil, fmt.Errorf("Jellyfin admin password missing in %s", namespace)
	}
	base := fmt.Sprintf("http://seerr.%s.svc.cluster.local", namespace)
	loginBody, _ := json.Marshal(map[string]any{
		"username":   user,
		"password":   password,
		"serverType": 2,
		"email":      user + "@servarr.local",
	})
	client := &http.Client{Timeout: 30 * time.Second}
	loginReq, _ := http.NewRequest(http.MethodPost, base+"/api/v1/auth/jellyfin", bytes.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := client.Do(loginReq)
	if err != nil {
		return "", nil, fmt.Errorf("Seerr login failed: %w", err)
	}
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("Seerr login failed (HTTP %d)", loginResp.StatusCode)
	}
	cookies := loginResp.Cookies()
	if len(cookies) == 0 {
		return "", nil, fmt.Errorf("Seerr login returned no session cookie")
	}
	return base, cookies, nil
}

func (in *Installer) flushSeerrCaches(namespace string) error {
	base, cookies, err := in.seerrSession(namespace)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, cache := range []string{"radarr", "sonarr"} {
		req, _ := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/api/v1/settings/cache/%s/flush", base, cache), nil)
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("Seerr %s cache flush failed: %w", cache, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("Seerr %s cache flush failed (HTTP %d)", cache, resp.StatusCode)
		}
	}
	return nil
}

func (in *Installer) arrProfileID(namespace, app, name string) (int, error) {
	profiles, err := in.arrQualityProfiles(namespace, app)
	if err != nil {
		return 0, err
	}
	for _, profile := range profiles {
		if mapString(profile, "name") == name {
			return mapInt(profile, "id"), nil
		}
	}
	return 0, fmt.Errorf("%s has no profile %q", app, name)
}

func (in *Installer) arrProfileUsage(namespace, app string, profileID int) (int, error) {
	path := "/api/v3/movie"
	if app == "sonarr" {
		path = "/api/v3/series"
	}
	status, data, err := in.arrRequest(namespace, app, http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("%s library listing failed (HTTP %d)", app, status)
	}
	rows, err := decodeObjectArray(data)
	if err != nil {
		return 0, err
	}
	used := 0
	for _, row := range rows {
		if mapInt(row, "qualityProfileId") == profileID {
			used++
		}
	}
	return used, nil
}

func (in *Installer) seerrProfileRequests(namespace string, profileIDs map[int]bool) (int, error) {
	base, cookies, err := in.seerrSession(namespace)
	if err != nil {
		return 0, err
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/request?take=100&filter=all", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, fmt.Errorf("Seerr request lookup failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Seerr request lookup failed (HTTP %d)", resp.StatusCode)
	}
	var body struct {
		Results []struct {
			Status    int `json:"status"`
			ProfileID int `json:"profileId"`
		} `json:"results"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return 0, fmt.Errorf("Seerr returned an unreadable request list")
	}
	used := 0
	for _, request := range body.Results {
		// Pending and approved requests still depend on the profile. Completed
		// requests may retain a historical profile id but no longer need it.
		if profileIDs[request.ProfileID] && (request.Status == 1 || request.Status == 2) {
			used++
		}
	}
	return used, nil
}

// DeleteServarrProfile removes one console-managed language/resolution profile
// from both apps. The base default and built-in profiles are never deletable.
func (in *Installer) DeleteServarrProfile(namespace, release, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("profile name is required")
	}
	settings, err := in.ServarrSettingsFor(release, namespace)
	if err != nil {
		return err
	}
	if name == servarrQualityBase(settings.QualityCutoff) || !strings.Contains(name, " · ") {
		return fmt.Errorf("the default profile and built-in profiles cannot be deleted")
	}
	ids := map[string]int{}
	for _, app := range []string{"radarr", "sonarr"} {
		id, err := in.arrProfileID(namespace, app, name)
		if err != nil {
			return err
		}
		ids[app] = id
		used, err := in.arrProfileUsage(namespace, app, id)
		if err != nil {
			return err
		}
		if used > 0 {
			return fmt.Errorf("%s profile %q is used by %d library item(s)", app, name, used)
		}
	}
	// Seerr stores the profile id on requests. Refuse to delete while a pending
	// or approved request still points at it.
	seerrUsed, err := in.seerrProfileRequests(namespace, map[int]bool{
		ids["radarr"]: true,
		ids["sonarr"]: true,
	})
	if err != nil {
		return err
	}
	if seerrUsed > 0 {
		return fmt.Errorf("profile %q is used by %d active Seerr request(s)", name, seerrUsed)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, app := range []string{"radarr", "sonarr"} {
		key, err := in.servarrArrAPIKey(namespace, app)
		if err != nil {
			return err
		}
		url := fmt.Sprintf("http://%s.%s.svc.cluster.local/api/v3/qualityprofile/%d", app, namespace, ids[app])
		req, _ := http.NewRequest(http.MethodDelete, url, nil)
		req.Header.Set("X-Api-Key", key)
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("%s profile delete failed: %w", app, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("%s profile delete failed (HTTP %d)", app, resp.StatusCode)
		}
	}
	if err := in.flushSeerrCaches(namespace); err != nil {
		return fmt.Errorf("profile deleted, but Seerr cache refresh failed: %w", err)
	}
	return nil
}
