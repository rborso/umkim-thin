package app

import (
	"fmt"
	"strings"
)

// giteaLanguageOptions is the subset of Gitea locales exposed by the console.
// Gitea uses the first locale as the fallback when the browser language does
// not match any entry.
var giteaLanguageOptions = []SettingOption{
	{Value: "en-US", Label: "English"},
	{Value: "hu-HU", Label: "Hungarian"},
	{Value: "de-DE", Label: "German"},
	{Value: "fr-FR", Label: "French"},
	{Value: "es-ES", Label: "Spanish"},
	{Value: "it-IT", Label: "Italian"},
	{Value: "nl-NL", Label: "Dutch"},
	{Value: "pl-PL", Label: "Polish"},
	{Value: "pt-BR", Label: "Portuguese (Brazil)"},
	{Value: "pt-PT", Label: "Portuguese (Portugal)"},
	{Value: "cs-CZ", Label: "Czech"},
	{Value: "ru-RU", Label: "Russian"},
	{Value: "uk-UA", Label: "Ukrainian"},
	{Value: "sv-SE", Label: "Swedish"},
	{Value: "fi-FI", Label: "Finnish"},
	{Value: "tr-TR", Label: "Turkish"},
	{Value: "el-GR", Label: "Greek"},
	{Value: "bg-BG", Label: "Bulgarian"},
	{Value: "ja-JP", Label: "Japanese"},
	{Value: "ko-KR", Label: "Korean"},
	{Value: "zh-CN", Label: "Chinese (Simplified)"},
	{Value: "zh-TW", Label: "Chinese (Traditional)"},
	{Value: "fa-IR", Label: "Persian"},
	{Value: "id-ID", Label: "Indonesian"},
}

// paperlessOCROptions are Tesseract language codes accepted by Paperless.
var paperlessOCROptions = []SettingOption{
	{Value: "eng", Label: "English"},
	{Value: "hun", Label: "Hungarian"},
	{Value: "deu", Label: "German"},
	{Value: "fra", Label: "French"},
	{Value: "spa", Label: "Spanish"},
	{Value: "ita", Label: "Italian"},
	{Value: "nld", Label: "Dutch"},
	{Value: "pol", Label: "Polish"},
	{Value: "ces", Label: "Czech"},
	{Value: "slk", Label: "Slovak"},
	{Value: "ron", Label: "Romanian"},
	{Value: "rus", Label: "Russian"},
	{Value: "ukr", Label: "Ukrainian"},
	{Value: "tur", Label: "Turkish"},
	{Value: "ell", Label: "Greek"},
	{Value: "por", Label: "Portuguese"},
	{Value: "swe", Label: "Swedish"},
	{Value: "dan", Label: "Danish"},
	{Value: "fin", Label: "Finnish"},
	{Value: "nor", Label: "Norwegian"},
	{Value: "jpn", Label: "Japanese"},
	{Value: "kor", Label: "Korean"},
	{Value: "chi_sim", Label: "Chinese (Simplified)"},
	{Value: "chi_tra", Label: "Chinese (Traditional)"},
	{Value: "ara", Label: "Arabic"},
	{Value: "heb", Label: "Hebrew"},
}

func normalizeGiteaLanguage(raw string) (string, error) {
	code := strings.TrimSpace(raw)
	for _, option := range giteaLanguageOptions {
		if strings.EqualFold(option.Value, code) {
			return option.Value, nil
		}
	}
	return "", fmt.Errorf("unsupported Gitea language %q", raw)
}

// giteaLanguageLists returns LANGS/NAMES with the chosen fallback first.
func giteaLanguageLists(fallback string) (string, string, error) {
	code, err := normalizeGiteaLanguage(fallback)
	if err != nil {
		return "", "", err
	}
	languages := make([]string, 0, len(giteaLanguageOptions))
	names := make([]string, 0, len(giteaLanguageOptions))
	for _, option := range giteaLanguageOptions {
		if option.Value == code {
			languages = append([]string{option.Value}, languages...)
			names = append([]string{option.Label}, names...)
			continue
		}
		languages = append(languages, option.Value)
		names = append(names, option.Label)
	}
	// The selected entry was appended in its original position above; remove
	// that duplicate while preserving the selected-first order.
	langs := make([]string, 0, len(giteaLanguageOptions))
	labels := make([]string, 0, len(giteaLanguageOptions))
	seen := map[string]bool{}
	for i, value := range languages {
		if seen[value] {
			continue
		}
		seen[value] = true
		langs = append(langs, value)
		labels = append(labels, names[i])
	}
	return strings.Join(langs, ","), strings.Join(labels, ","), nil
}

func normalizeOCRLanguage(raw string) (string, error) {
	code := strings.ToLower(strings.TrimSpace(raw))
	for _, option := range paperlessOCROptions {
		if option.Value == code {
			return code, nil
		}
	}
	return "", fmt.Errorf("unsupported OCR language %q", raw)
}
