package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/rborso/umkim-thin/internal/app"
	"github.com/rborso/umkim-thin/internal/backup"
	"github.com/rborso/umkim-thin/internal/catalog"
	"github.com/rborso/umkim-thin/internal/cert"
	"github.com/rborso/umkim-thin/internal/identity"
	"github.com/rborso/umkim-thin/internal/storage"
	"github.com/rborso/umkim-thin/internal/update"
	"github.com/rborso/umkim-thin/internal/wg"
)

//go:embed ui
var uiFS embed.FS

type installRequest struct {
	Name            string                            `json:"name"`
	Host            string                            `json:"host"`
	Hosts           map[string]string                 `json:"hosts"`
	ConfigClass     string                            `json:"configClass"`
	MediaClass      string                            `json:"mediaClass"`
	MediaAccessMode string                            `json:"mediaAccessMode"`
	Namespace       string                            `json:"namespace"`
	PVCs            map[string]app.PVCOverride        `json:"pvcs"`
	Resources       map[string]app.ContainerResources `json:"resources"`
	Version         string                            `json:"version"`
	Exposure        map[string]string                 `json:"exposure"`
	AllowLocal      map[string]bool                   `json:"allowLocal"`
	Certificate     map[string]string                 `json:"certificate"`
	MediaLanguage   string                            `json:"mediaLanguage"`
	QualityCutoff   string                            `json:"qualityCutoff"`
	ExtraLanguages  []string                          `json:"extraLanguages"`
	Language        string                            `json:"language"`
	OCRLanguage     string                            `json:"ocrLanguage"`
}

func (r installRequest) options() app.Options {
	opts := app.Options{
		Hosts: r.Hosts, ConfigClass: r.ConfigClass, MediaClass: r.MediaClass,
		MediaAccessMode: r.MediaAccessMode,
		Namespace:       r.Namespace, PVCs: r.PVCs, Resources: r.Resources,
		ChartVersion: r.Version, Exposure: r.Exposure, AllowLocal: r.AllowLocal,
		Certificate: r.Certificate, MediaLanguage: r.MediaLanguage,
		QualityCutoff: r.QualityCutoff, ExtraLanguages: r.ExtraLanguages,
		Language: r.Language, OCRLanguage: r.OCRLanguage,
	}
	if opts.Hosts == nil {
		opts.Hosts = map[string]string{}
	}
	if r.Host != "" {
		opts.Hosts[""] = r.Host
	}
	return opts
}

// serveAPI runs the API and the console from one process.
//
// The endpoints mirror the CLI: the console is a client of the same API, not a
// second implementation of the same logic.
func serveAPI(catalogDir, addr, homeAddr string) error {
	entries, err := catalog.Load(catalogDir)
	if err != nil {
		return err
	}
	byName := map[string]catalog.Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/catalog", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, entries)
	})

	mux.HandleFunc("GET /api/catalog/{name}", func(w http.ResponseWriter, r *http.Request) {
		e, ok := byName[r.PathValue("name")]
		if !ok {
			http.Error(w, "no such catalog entry", http.StatusNotFound)
			return
		}
		writeJSON(w, e)
	})

	// Scaffold: these are the operations the product is built around, and the
	// CLI calls the same handlers.
	cfg := app.ConfigFromEnv()
	installer := app.New(cfg)
	certs := cert.New(cert.Config{
		Kubectl: cfg.Kubectl, Kubeconfig: cfg.Kubeconfig, TraefikNS: cfg.TraefikNS,
		Helm: cfg.Helm, IdentityNS: cfg.IdentityNS, IdentityChart: cfg.IdentityChart,
		IdentityValues: cfg.IdentityValues, IdentityApps: cfg.IdentityApps,
		IdentityCerts: filepath.Join(cfg.StateDir, "identity-certs.yml"),
	})
	directory := identity.New(identity.ConfigFromEnv())
	storageSock := envOr("UMKIM_THIN_STORAGE_SOCKET", "/run/umkim-thin/storage.sock")
	storageAPI := &storageAPI{socket: storageSock, http: unixHTTPClient(storageSock)}
	// External NFS does not need the host agent: the console installs the
	// provisioner and records the export on its own PVC. This also makes it
	// usable on HA, where the single-node disk agent is intentionally absent.
	externalStorage := storage.New(storage.Config{
		StateFile:  filepath.Join(cfg.StateDir, "storage-external.yml"),
		Helm:       cfg.Helm,
		Kubectl:    cfg.Kubectl,
		Kubeconfig: cfg.Kubeconfig,
	})
	auth := newConsoleAuth()
	jobs := newJobStore()
	started := time.Now()

	// The catalog and the version ship inside the image, so the console asks the
	// public release metadata whether a newer one exists. Read-only, cached, and
	// silent when it fails; UMKIM_THIN_UPDATE_CHECK=false disables it for an
	// air-gapped install.
	updates := update.New(update.Config{
		Current:   cfg.Version,
		Enabled:   envBool("UMKIM_THIN_UPDATE_CHECK", true),
		Endpoint:  envOr("UMKIM_THIN_UPDATE_CHECK_URL", update.DefaultEndpoint),
		StateFile: filepath.Join(cfg.StateDir, "update-check.json"),
	})
	go updates.Start(context.Background())

	// Backups: one archive per run (console state, installer seed, app
	// inventory) plus a VolumeSnapshot per application PVC. The schedule runs
	// in the background; a restart does not trigger an immediate backup.
	backups := backup.New(backup.Config{
		Dir:        cfg.BackupDir,
		StateDir:   cfg.StateDir,
		SeedDir:    cfg.BackupSeedDir,
		Helm:       cfg.Helm,
		Kubectl:    cfg.Kubectl,
		Kubeconfig: cfg.Kubeconfig,
		Version:    cfg.Version,
		Zone:       cfg.Zone,
		Keep:       cfg.BackupKeep,
		Snapshots:  cfg.BackupSnapshots,
	})
	if cfg.BackupInterval > 0 {
		go func() {
			t := time.NewTicker(cfg.BackupInterval)
			defer t.Stop()
			for range t.C {
				a, err := backups.Create(context.Background(), "scheduled")
				if err != nil {
					log.Printf("backup: scheduled run failed: %v", err)
					continue
				}
				log.Printf("backup: wrote %s (%d files, %d snapshots)", a.Name, a.Files, a.Snapshots)
			}
		}()
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := installer.Ready(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, "# TYPE umkim_thin_up gauge\numkim_thin_up 1\n")
		fmt.Fprintf(w, "# TYPE umkim_thin_uptime_seconds gauge\numkim_thin_uptime_seconds %d\n", int(time.Since(started).Seconds()))
		fmt.Fprintf(w, "# TYPE umkim_thin_goroutines gauge\numkim_thin_goroutines %d\n", runtime.NumGoroutine())
		fmt.Fprintf(w, "# TYPE umkim_thin_memory_alloc_bytes gauge\numkim_thin_memory_alloc_bytes %d\n", ms.Alloc)
		fmt.Fprintf(w, "# TYPE umkim_thin_installs_total counter\numkim_thin_installs_total %d\n", jobs.installs.Load())
		fmt.Fprintf(w, "# TYPE umkim_thin_install_errors_total counter\numkim_thin_install_errors_total %d\n", jobs.errors.Load())
	})

	loginHTML, _ := fs.ReadFile(uiFS, "ui/login.html")
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(loginHTML)
	})
	mux.HandleFunc("POST /login", auth.login)
	mux.HandleFunc("GET /logout", auth.logout)

	mux.HandleFunc("GET /api/apps", func(w http.ResponseWriter, r *http.Request) {
		installed, err := installer.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Helm knows about the platform's own releases too (identity, the
		// console, ...). The console lists catalog apps: those are the ones it
		// may manage.
		out := make([]app.Result, 0, len(installed))
		for _, a := range installed {
			if e, ok := byName[a.Name]; ok {
				a.License = e.License
				a.Links = e.Links
				out = append(out, a)
			}
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/apps/servarr/settings", func(w http.ResponseWriter, r *http.Request) {
		settings, err := installer.ServarrSettings()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"mediaLanguage": settings.MediaLanguage,
			"qualityCutoff": settings.QualityCutoff,
			"languages":     app.ServarrLanguageOptions(),
			"qualities":     app.ServarrQualityOptions(),
		})
	})
	servarrTarget := func(w http.ResponseWriter, r *http.Request) (catalog.Entry, string, bool) {
		name := r.PathValue("name")
		e, ok := byName[name]
		if !ok || e.Name != app.ServarrApp {
			http.Error(w, "servarr app not found", http.StatusNotFound)
			return catalog.Entry{}, "", false
		}
		namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))
		if namespace == "" {
			namespace = e.Namespace
		}
		if namespace != e.Namespace {
			// A second Servarr release can live in another namespace. Accept
			// only a namespace that actually holds that named release.
			installed, err := installer.List()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return catalog.Entry{}, "", false
			}
			found := false
			for _, item := range installed {
				if item.Name == name && item.Namespace == namespace {
					found = true
					break
				}
			}
			if !found {
				http.Error(w, "servarr app not found in that namespace", http.StatusNotFound)
				return catalog.Entry{}, "", false
			}
		}
		return e, namespace, true
	}
	mux.HandleFunc("GET /api/apps/{name}/indexers", func(w http.ResponseWriter, r *http.Request) {
		_, namespace, ok := servarrTarget(w, r)
		if !ok {
			return
		}
		indexers, err := installer.ListProwlarrIndexers(namespace)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, indexers)
	})
	mux.HandleFunc("GET /api/apps/{name}/indexer-definitions", func(w http.ResponseWriter, r *http.Request) {
		_, namespace, ok := servarrTarget(w, r)
		if !ok {
			return
		}
		definitions, err := installer.SearchProwlarrDefinitions(namespace, r.URL.Query().Get("q"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, definitions)
	})
	mux.HandleFunc("GET /api/apps/{name}/indexer-definition", func(w http.ResponseWriter, r *http.Request) {
		_, namespace, ok := servarrTarget(w, r)
		if !ok {
			return
		}
		definition := strings.TrimSpace(r.URL.Query().Get("definition"))
		if definition == "" {
			http.Error(w, "definition is required", http.StatusBadRequest)
			return
		}
		def, err := installer.ProwlarrDefinition(namespace, definition)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, def)
	})
	mutating(mux, "POST /api/apps/{name}/indexers", func(w http.ResponseWriter, r *http.Request) {
		_, namespace, ok := servarrTarget(w, r)
		if !ok {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
		var req app.IndexerRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		indexer, err := installer.AddProwlarrIndexer(namespace, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		audit(r, auth, "indexer-add", req.DefinitionName, fmt.Sprintf("name=%q namespace=%q", indexer.Name, namespace))
		writeJSON(w, indexer)
	})
	mux.HandleFunc("GET /api/apps/{name}/profiles", func(w http.ResponseWriter, r *http.Request) {
		_, namespace, ok := servarrTarget(w, r)
		if !ok {
			return
		}
		profiles, err := installer.ListServarrProfiles(namespace, r.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, profiles)
	})
	mutating(mux, "POST /api/apps/{name}/profiles", func(w http.ResponseWriter, r *http.Request) {
		_, namespace, ok := servarrTarget(w, r)
		if !ok {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req app.MediaProfileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		settings, err := installer.ServarrSettingsFor(r.PathValue("name"), namespace)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		quality := req.QualityCutoff
		if quality == "" {
			quality = settings.QualityCutoff
		}
		if err := installer.EnsureServarrProfiles(namespace, r.PathValue("name"),
			quality, settings.MediaLanguage, []string{req.Language}, nil); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		audit(r, auth, "media-profile-add", r.PathValue("name"),
			fmt.Sprintf("language=%s quality=%s namespace=%s", req.Language, quality, namespace))
		profiles, err := installer.ListServarrProfiles(namespace, r.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, profiles)
	})
	mutating(mux, "DELETE /api/apps/{name}/profiles", func(w http.ResponseWriter, r *http.Request) {
		_, namespace, ok := servarrTarget(w, r)
		if !ok {
			return
		}
		profile := strings.TrimSpace(r.URL.Query().Get("profile"))
		if profile == "" {
			http.Error(w, "profile is required", http.StatusBadRequest)
			return
		}
		if err := installer.DeleteServarrProfile(namespace, r.PathValue("name"), profile); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		audit(r, auth, "media-profile-delete", r.PathValue("name"), fmt.Sprintf("profile=%q namespace=%s", profile, namespace))
		w.WriteHeader(http.StatusNoContent)
	})
	mutating(mux, "POST /api/apps/servarr/settings", func(w http.ResponseWriter, r *http.Request) {
		var req app.ServarrSettings
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		settings, err := app.NormalizeServarrSettings(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		e, ok := byName[app.ServarrApp]
		if !ok {
			http.Error(w, "no such catalog entry", http.StatusNotFound)
			return
		}
		audit(r, auth, "servarr-defaults", app.ServarrApp,
			fmt.Sprintf("language=%s quality=%s", settings.MediaLanguage, settings.QualityCutoff))
		writeJSON(w, jobs.start("setup", app.ServarrApp, func(progress func(string)) (any, error) {
			return installer.SetupServarr(e, settings, progress)
		}))
	})
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		cfg := app.ConfigFromEnv()
		classes, _ := installer.StorageClasses()
		names := make([]string, 0, len(classes))
		for _, c := range classes {
			names = append(names, c.Name)
		}
		writeJSON(w, map[string]any{
			"zone":                          cfg.Zone,
			"portalHost":                    cfg.PortalHost,
			"homeUrl":                       cfg.HomeURL(),
			"directoryUrl":                  cfg.DirectoryURL,
			"version":                       cfg.Version,
			"configClass":                   cfg.ConfigClass,
			"mediaClass":                    cfg.MediaClass,
			"mediaAccessMode":               cfg.MediaAccessMode,
			"storageClasses":                names,
			"storageClassInfo":              classes,
			"trustedAccessEnabled":          cfg.TrustedAccessEnabled,
			"trustedManagementEnabled":      cfg.TrustedManagementEnabled,
			"trustedManagementCIDRs":        cfg.TrustedManagementCIDRs,
			"trustedManagementExcludeCIDRs": cfg.TrustedManagementExcludeCIDRs,
		})
	})
	mux.HandleFunc("GET /api/version", versionHandler(updates))
	mutating(mux, "POST /api/apps", func(w http.ResponseWriter, r *http.Request) {
		var req installRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		e, ok := byName[req.Name]
		if !ok {
			http.Error(w, "no such catalog entry", http.StatusNotFound)
			return
		}
		opts := req.options()
		if e.Name == app.ServarrApp {
			settings, err := app.NormalizeServarrSettings(app.ServarrSettings{
				MediaLanguage: req.MediaLanguage,
				QualityCutoff: req.QualityCutoff,
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			opts.MediaLanguage = settings.MediaLanguage
			opts.QualityCutoff = settings.QualityCutoff
		}
		audit(r, auth, "install", req.Name, "")
		writeJSON(w, jobs.start("install", req.Name, func(progress func(string)) (any, error) {
			opts.Progress = progress
			res, err := installer.Install(e, opts)
			if err != nil {
				return nil, err
			}
			// The media stack configures itself; a bootstrap failure never fails
			// the install: the app is healthy and the Applications section offers
			// a retry.
			if e.Name == app.ServarrApp {
				progress("running the media stack bootstrap")
				settings, err := installer.ServarrSettings()
				if err != nil {
					progress("could not read media defaults (the app stays installed): " + err.Error())
				} else if _, err := installer.SetupServarr(e, settings, progress); err != nil {
					progress("bootstrap failed (the app stays installed): " + err.Error())
				} else if len(opts.ExtraLanguages) > 0 {
					if err := installer.EnsureServarrProfiles(e.Namespace, e.Name,
						opts.QualityCutoff, opts.MediaLanguage, opts.ExtraLanguages, progress); err != nil {
						progress("optional media profiles failed (the app stays installed): " + err.Error())
					}
				}
			}
			return res, nil
		}))
	})
	mux.HandleFunc("GET /api/apps/{name}/storage", func(w http.ResponseWriter, r *http.Request) {
		e, ok := byName[r.PathValue("name")]
		if !ok {
			http.Error(w, "no such catalog entry", http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		plan, err := installer.StoragePlan(e, app.Options{
			Namespace:       q.Get("namespace"),
			ConfigClass:     q.Get("configClass"),
			MediaClass:      q.Get("mediaClass"),
			MediaAccessMode: q.Get("mediaAccessMode"),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, plan)
	})
	mux.HandleFunc("GET /api/apps/{name}/plan", func(w http.ResponseWriter, r *http.Request) {
		e, ok := byName[r.PathValue("name")]
		if !ok {
			http.Error(w, "no such catalog entry", http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		plan, err := installer.RenderPlan(e, app.Options{
			Namespace:       q.Get("namespace"),
			ConfigClass:     q.Get("configClass"),
			MediaClass:      q.Get("mediaClass"),
			MediaAccessMode: q.Get("mediaAccessMode"),
			ChartVersion:    q.Get("version"),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, plan)
	})
	mux.HandleFunc("GET /api/apps/{name}/release", func(w http.ResponseWriter, r *http.Request) {
		e, ok := byName[r.PathValue("name")]
		if !ok {
			http.Error(w, "no such catalog entry", http.StatusNotFound)
			return
		}
		info, err := installer.Release(e, r.URL.Query().Get("namespace"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, info)
	})
	mux.HandleFunc("POST /api/apps/{name}/validate", func(w http.ResponseWriter, r *http.Request) {
		e, ok := byName[r.PathValue("name")]
		if !ok {
			http.Error(w, "no such catalog entry", http.StatusNotFound)
			return
		}
		var req installRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		opts := req.options()
		result, err := installer.Validate(e, opts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, result)
	})
	mux.HandleFunc("GET /api/apps/{name}/versions", func(w http.ResponseWriter, r *http.Request) {
		e, ok := byName[r.PathValue("name")]
		if !ok {
			http.Error(w, "no such catalog entry", http.StatusNotFound)
			return
		}
		vi, err := installer.Versions(e)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, vi)
	})
	mutating(mux, "POST /api/apps/{name}/upgrade", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		e, ok := byName[name]
		if !ok {
			http.Error(w, "no such catalog entry", http.StatusNotFound)
			return
		}
		var req struct {
			Version       string `json:"version"`
			ConfirmBackup bool   `json:"confirmBackup"`
			SkipBackup    bool   `json:"skipBackup"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Version == "" {
			http.Error(w, "version is required", http.StatusBadRequest)
			return
		}
		if !req.ConfirmBackup && !req.SkipBackup {
			http.Error(w, "confirm the pre-upgrade backup (or set skipBackup)", http.StatusBadRequest)
			return
		}
		audit(r, auth, "upgrade", name, "to="+req.Version)
		writeJSON(w, jobs.start("upgrade", name, func(progress func(string)) (any, error) {
			return installer.Install(e, app.Options{
				ChartVersion: req.Version, Upgrade: true,
				ConfirmBackup: req.ConfirmBackup, SkipBackup: req.SkipBackup,
				Progress: progress,
			})
		}))
	})
	mux.HandleFunc("GET /api/certificates", func(w http.ResponseWriter, r *http.Request) {
		infos, err := certs.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, infos)
	})
	mutating(mux, "POST /api/certificates", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name      string   `json:"name"`
			Namespace string   `json:"namespace"`
			Cert      string   `json:"cert"`
			Key       string   `json:"key"`
			Hosts     []string `json:"hosts"`
			Default   bool     `json:"default"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.Cert == "" || req.Key == "" {
			http.Error(w, "name, cert and key are required", http.StatusBadRequest)
			return
		}
		info, err := certs.Add(req.Name, req.Namespace, []byte(req.Cert), []byte(req.Key), req.Hosts, req.Default)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		audit(r, auth, "certificate-add", req.Name, "namespace="+info.Namespace)
		writeJSON(w, info)
	})
	mutating(mux, "POST /api/certificates/{name}/default", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := certs.SetDefault(name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		audit(r, auth, "certificate-default", name, "")
		writeJSON(w, map[string]string{"status": "default", "name": name})
	})
	mutating(mux, "DELETE /api/certificates/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		namespace := r.URL.Query().Get("namespace")
		if err := certs.Remove(name, namespace); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		audit(r, auth, "certificate-remove", name, "namespace="+namespace)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/storage", func(w http.ResponseWriter, r *http.Request) {
		records := externalStorage.List()
		if storageAPI.available() {
			var host []storage.Record
			if err := storageAPI.get("/state", &host); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			records = append(host, records...)
		}
		if records == nil {
			records = []storage.Record{}
		}
		writeJSON(w, records)
	})
	mux.HandleFunc("GET /api/storage/disks", func(w http.ResponseWriter, r *http.Request) {
		if !storageAPI.available() {
			http.Error(w, "local storage agent is not available on this node", http.StatusServiceUnavailable)
			return
		}
		var disks []storage.Disk
		if err := storageAPI.get("/disks", &disks); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, disks)
	})
	mutating(mux, "POST /api/storage/plan", func(w http.ResponseWriter, r *http.Request) {
		var req storage.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		var plan storage.Plan
		var err error
		if req.Kind == "external" {
			plan, err = externalStorage.Plan(req)
		} else {
			if !storageAPI.available() {
				http.Error(w, "local storage agent is not available on this node", http.StatusServiceUnavailable)
				return
			}
			err = storageAPI.post("/plan", req, &plan)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, plan)
	})
	// The plan can only check reachability and the export list; this mounts the
	// export in a throwaway pod, the same way the provisioner will.
	mutating(mux, "POST /api/storage/verify", func(w http.ResponseWriter, r *http.Request) {
		var req storage.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Kind != "external" {
			http.Error(w, "only external NFS exports can be verified", http.StatusBadRequest)
			return
		}
		server := strings.TrimSpace(req.Server)
		if server == "" {
			server = externalStorage.NFSServer
		}
		mountOptions := strings.TrimSpace(req.MountOptions)
		if mountOptions == "" {
			mountOptions = "nfsvers=4.2,rw,nolock"
		}
		warnings, err := externalStorage.ProbeMount(server, strings.TrimSpace(req.Export), mountOptions)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "warnings": warnings})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "warnings": warnings})
	})
	mutating(mux, "POST /api/storage/apply", func(w http.ResponseWriter, r *http.Request) {
		var req storage.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Name != "" {
			audit(r, auth, "storage-apply", req.Name, req.Kind)
		}
		var plan storage.Plan
		var err error
		if req.Kind == "external" {
			// The host agent and the console keep separate state. Refuse a
			// duplicate name so one Helm release cannot own two exports.
			if storageAPI.available() {
				var host []storage.Record
				if e := storageAPI.get("/state", &host); e == nil {
					for _, x := range host {
						if x.Name == req.Name {
							http.Error(w, "that storage name is already used by a local export", http.StatusConflict)
							return
						}
					}
				}
			}
			plan, err = externalStorage.Apply(req)
		} else {
			if !storageAPI.available() {
				http.Error(w, "local storage agent is not available on this node", http.StatusServiceUnavailable)
				return
			}
			for _, x := range externalStorage.List() {
				if x.Name == req.Name {
					http.Error(w, "that storage name is already used by an external NFS export", http.StatusConflict)
					return
				}
			}
			err = storageAPI.post("/apply", req, &plan)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, plan)
	})
	mutating(mux, "DELETE /api/storage/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		audit(r, auth, "storage-remove", name, "")
		external := false
		for _, x := range externalStorage.List() {
			if x.Name == name {
				external = true
				break
			}
		}
		var err error
		if external {
			err = externalStorage.Remove(name)
		} else {
			if !storageAPI.available() {
				http.Error(w, "local storage agent is not available on this node", http.StatusServiceUnavailable)
				return
			}
			err = storageAPI.post("/remove", map[string]string{"name": name}, nil)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Backups. The list is cheap (it reads each manifest), so it is not cached;
	// a run and a restore are mutations and audited.
	mux.HandleFunc("GET /api/backups", func(w http.ResponseWriter, r *http.Request) {
		list, err := backups.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cfg := app.ConfigFromEnv()
		resp := map[string]any{
			"dir":       backups.Dir(),
			"keep":      cfg.BackupKeep,
			"snapshots": cfg.BackupSnapshots,
			"enabled":   cfg.BackupInterval > 0,
			"interval":  cfg.BackupInterval.String(),
			"archives":  list,
		}
		if len(list) > 0 {
			resp["last"] = list[0].Created
			if cfg.BackupInterval > 0 {
				resp["next"] = list[0].Created.Add(cfg.BackupInterval)
			}
		}
		writeJSON(w, resp)
	})
	mutating(mux, "POST /api/backups", func(w http.ResponseWriter, r *http.Request) {
		a, err := backups.Create(r.Context(), "manual")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		audit(r, auth, "backup-create", a.Name, fmt.Sprintf("%d files, %d snapshots", a.Files, a.Snapshots))
		writeJSON(w, a)
	})
	mux.HandleFunc("GET /api/backups/{name}/download", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		p, err := backups.Path(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		http.ServeFile(w, r, p)
	})
	mutating(mux, "POST /api/backups/{name}/restore", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Confirm bool `json:"confirm"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Confirm {
			http.Error(w, "confirm the restore (it replaces the console state; a safety archive is written first)", http.StatusBadRequest)
			return
		}
		name := r.PathValue("name")
		rep, err := backups.Restore(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		audit(r, auth, "backup-restore", name, fmt.Sprintf("%d files, safety %s", len(rep.Restored), rep.SafetyArchive))
		writeJSON(w, rep)
	})
	mutating(mux, "DELETE /api/backups/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := backups.Delete(name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		audit(r, auth, "backup-delete", name, "")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, jobs.list())
	})
	mux.HandleFunc("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, ok := jobs.get(r.PathValue("id"))
		if !ok {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		writeJSON(w, j)
	})
	mux.HandleFunc("GET /api/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var entry *catalog.Entry
		if e, ok := byName[name]; ok {
			entry = &e
		}
		st, err := installer.Status(name, entry)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, st)
	})

	// One poll for the whole Applications section. It shells out per app, so
	// the answer is cached; the UI polls every two minutes, well above this.
	var statusMu sync.Mutex
	var statusAt time.Time
	var statusData []appStatus
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		statusMu.Lock()
		if statusData != nil && time.Since(statusAt) < 45*time.Second {
			data := statusData
			statusMu.Unlock()
			writeJSON(w, data)
			return
		}
		statusMu.Unlock()

		installed, err := installer.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out := make([]appStatus, 0, len(installed))
		for _, a := range installed {
			entry, ok := byName[a.Name]
			if !ok {
				continue // a platform release, not a catalog app
			}
			st, err := installer.Status(a.Name, &entry)
			if err != nil {
				continue
			}
			latestApp := ""
			for _, v := range entry.Chart.Versions {
				if v.Version == entry.Chart.Version {
					latestApp = v.AppVersion
					break
				}
			}
			se := appStatus{
				Status:          st,
				LatestChart:     entry.Chart.Version,
				LatestApp:       latestApp,
				UpdateAvailable: updateAvailable(st.Chart, entry.Chart.Version),
			}
			if a.Name == app.ServarrApp {
				se.Bootstrap = installer.ServarrBootstrapState()
			}
			out = append(out, se)
		}
		statusMu.Lock()
		statusData = out
		statusAt = time.Now()
		statusMu.Unlock()
		writeJSON(w, out)
	})

	// The media stack's bootstrap no longer depends on tracker credentials: it
	// wires the stack's own services. Only the media stack has one today.
	mutating(mux, "POST /api/apps/{name}/setup", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name != app.ServarrApp {
			http.Error(w, "setup is not implemented for this app", http.StatusNotFound)
			return
		}
		e, ok := byName[name]
		if !ok {
			http.Error(w, "no such catalog entry", http.StatusNotFound)
			return
		}
		settings, err := installer.ServarrSettings()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		audit(r, auth, "servarr-setup", name, "")
		writeJSON(w, jobs.start("setup", name, func(progress func(string)) (any, error) {
			return installer.SetupServarr(e, settings, progress)
		}))
	})
	// Optional, explicit integration: create a Mealie API token and register a
	// global Mealie food provider in SparkyFitness. It is one click: the console
	// reads the two release Secrets, encrypts the token with SparkyFitness's own
	// key, and writes the provider row. Running it again is a no-op when the
	// provider already points at this Mealie release.
	mutating(mux, "POST /api/apps/{name}/integrations/mealie", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") != "sparkyfitness" {
			http.Error(w, "integration is only available for SparkyFitness", http.StatusNotFound)
			return
		}
		audit(r, auth, "mealie-sparkyfitness", "sparkyfitness", "")
		res, err := installer.ConnectMealieToSparky()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, res)
	})
	mutating(mux, "DELETE /api/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		e, ok := byName[name]
		if !ok {
			http.Error(w, "no such catalog entry", http.StatusNotFound)
			return
		}
		purge := r.URL.Query().Get("purge") == "true" || r.URL.Query().Get("purge") == "1"
		audit(r, auth, "uninstall", name, fmt.Sprintf("purge=%t", purge))
		if err := installer.Uninstall(name, &e, purge); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/invites", func(w http.ResponseWriter, r *http.Request) {
		invites, err := directory.ListInvites()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, invites)
	})
	mutating(mux, "POST /api/invites", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Email       string `json:"email"`
			DisplayName string `json:"displayName"`
			Group       string `json:"group"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		audit(r, auth, "invite-create", req.Email, "group="+req.Group)
		inv, link, err := directory.CreateInvite(req.Email, req.DisplayName, req.Group)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"invite": inv, "link": link})
	})
	// The invite sign-up page posts here (through the identity chart's nginx).
	mux.HandleFunc("POST /api/invites/redeem", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token       string `json:"token"`
			Username    string `json:"username"`
			Password    string `json:"password"`
			DisplayName string `json:"displayName"`
			Email       string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		key := "redeem:" + clientKey(r)
		if !auth.redeemLimiter.allow(key) {
			http.Error(w, "too many attempts; try again later", http.StatusTooManyRequests)
			return
		}
		if err := directory.Redeem(req.Token, req.Username, req.Password, req.DisplayName, req.Email); err != nil {
			auth.redeemLimiter.fail(key)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		auth.redeemLimiter.reset(key)
		writeJSON(w, map[string]string{"username": req.Username, "status": "created"})
	})
	mutating(mux, "DELETE /api/invites/{token}", func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		audit(r, auth, "invite-revoke", tokenPrefix(token), "")
		if err := directory.Revoke(token); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/directory/groups", func(w http.ResponseWriter, r *http.Request) {
		groups, err := directory.Groups()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if groups == nil {
			groups = []string{}
		}
		writeJSON(w, groups)
	})
	// CI cleanup only. General directory user management is deliberately not a
	// console feature: the CI accounts are the ones with fixed, public names.
	mutating(mux, "DELETE /api/directory/users/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !strings.HasPrefix(name, "ci-") {
			http.Error(w, "only ci-* accounts can be deleted through this endpoint", http.StatusForbidden)
			return
		}
		if err := directory.DeleteUser(name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		audit(r, auth, "delete-ci-user", name, "")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/wg/clients", func(w http.ResponseWriter, r *http.Request) {
		peers, err := wg.New(wg.ConfigFromEnv()).List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if peers == nil {
			peers = []wg.Peer{}
		}
		writeJSON(w, peers)
	})
	mutating(mux, "POST /api/wg/clients", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		audit(r, auth, "wg-create", req.Name, "")
		c, err := wg.New(wg.ConfigFromEnv()).Create(req.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := map[string]any{
			"name": c.Name, "address": c.Address, "publicKey": c.PublicKey, "config": c.Config,
		}
		// The config is ~400 bytes; keep the QR large enough that phone
		// scanners do not misread a section header.
		if png, err := qrcode.Encode(c.Config, qrcode.Medium, 512); err == nil {
			resp["qr"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
		}
		writeJSON(w, resp)
	})
	mutating(mux, "DELETE /api/wg/clients/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		audit(r, auth, "wg-delete", name, "")
		if err := wg.New(wg.ConfigFromEnv()).Delete(name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	static, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(static)))

	if homeAddr != "" {
		homePage, err := fs.ReadFile(uiFS, "ui/home.html")
		if err != nil {
			return err
		}
		homeMux := http.NewServeMux()
		homeMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("ok\n"))
		})
		homeMux.HandleFunc("GET /api/home", homeAPIHandler(installer, entries, cfg.DirectoryURL))
		homeMux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Robots-Tag", "noindex")
			_, _ = w.Write(homePage)
		})
		homeSrv := newHTTPServer(homeAddr, securityHeaders(requireRemoteUser(homeMux)))
		go func() {
			log.Printf("home listening on %s", homeAddr)
			if err := homeSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("home server stopped: %v", err)
			}
		}()
	}

	fmt.Printf("umkim-thin listening on %s (%d catalog entries)\n", addr, len(entries))
	srv := newHTTPServer(addr, securityHeaders(auth.requireAuth(mux)))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// requireRemoteUser keeps the home port from being a direct cluster-internal
// bypass. The home Ingress attaches Authelia's forward-auth middleware, which
// adds Remote-User; a request straight to the ClusterIP does not carry it.
func requireRemoteUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.TrimSpace(r.Header.Get("Remote-User")) == "" {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// homeApp is the deliberately small, user-facing view of an installed app.
// Namespaces, Helm phases, pod counts and mutations stay in the admin console.
type homeApp struct {
	Name        string         `json:"name"`
	DisplayName string         `json:"displayName"`
	Description string         `json:"description,omitempty"`
	URL         string         `json:"url"`
	Status      string         `json:"status"` // available | starting | unavailable
	VPN         bool           `json:"vpn,omitempty"`
	Links       *catalog.Links `json:"links,omitempty"`
}

type homeResponse struct {
	User string `json:"user,omitempty"`
	// DirectoryURL is the LLDAP web UI, where the user changes their own
	// password. Empty when the zone is unknown.
	DirectoryURL string    `json:"directoryUrl,omitempty"`
	Apps         []homeApp `json:"apps"`
}

// homeAPIHandler returns installed catalog apps with their public routes. The
// expensive Helm/kubectl work runs in the background: the first successful
// build is shared by all users, and a stale snapshot is served immediately
// while the next one is refreshed.
func homeAPIHandler(installer *app.Installer, entries []catalog.Entry, directoryURL string) http.HandlerFunc {
	store := &homeStore{
		build: func() []homeApp { return buildHomeApps(installer, entries) },
		ready: make(chan struct{}),
	}
	store.start()
	return func(w http.ResponseWriter, r *http.Request) {
		// The first request waits for the initial warm-up; later requests use
		// the snapshot and never block on Helm/kubectl.
		select {
		case <-store.ready:
		default:
			<-store.ready
		}
		apps := store.snapshot()
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, homeResponse{
			User:         strings.TrimSpace(r.Header.Get("Remote-User")),
			DirectoryURL: directoryURL,
			Apps:         apps,
		})
	}
}

type homeStore struct {
	mu          sync.Mutex
	build       func() []homeApp
	apps        []homeApp
	at          time.Time
	refreshing  bool
	ready       chan struct{}
	initialized bool
}

func (s *homeStore) start() {
	s.mu.Lock()
	s.refreshing = true
	s.mu.Unlock()
	go func() {
		s.refresh()
		s.mu.Lock()
		s.refreshing = false
		s.mu.Unlock()
		close(s.ready)
	}()
}

func (s *homeStore) refresh() {
	apps := s.build()
	s.mu.Lock()
	s.apps = apps
	s.at = time.Now()
	s.initialized = true
	s.mu.Unlock()
}

func (s *homeStore) snapshot() []homeApp {
	s.mu.Lock()
	if s.initialized && time.Since(s.at) > 60*time.Second && !s.refreshing {
		s.refreshing = true
		go func() {
			s.refresh()
			s.mu.Lock()
			s.refreshing = false
			s.mu.Unlock()
		}()
	}
	apps := append([]homeApp(nil), s.apps...)
	s.mu.Unlock()
	return apps
}

func buildHomeApps(installer *app.Installer, entries []catalog.Entry) []homeApp {
	installed, err := installer.List()
	if err != nil {
		log.Printf("home: listing installed apps: %v", err)
		return []homeApp{}
	}
	byName := make(map[string]app.Result, len(installed))
	for _, a := range installed {
		byName[a.Name] = a
	}
	entryByName := make(map[string]catalog.Entry, len(entries))
	for _, e := range entries {
		entryByName[e.Name] = e
	}

	out := make([]homeApp, 0, len(entries))
	for _, e := range entries {
		a, ok := byName[e.Name]
		if !ok || len(a.Routes) == 0 {
			continue
		}
		displayName := e.DisplayName
		if displayName == "" {
			displayName = e.Name
		}
		out = append(out, homeApp{
			Name:        e.Name,
			DisplayName: displayName,
			Description: e.Description,
			URL:         "https://" + a.Routes[0],
			Links:       e.Links,
		})
	}

	// Each Status call shells out to Helm and kubectl. Run them concurrently;
	// a household home page should not wait for six sequential process chains.
	var wg sync.WaitGroup
	for i := range out {
		i := i
		e := entryByName[out[i].Name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := installer.Status(e.Name, &e)
			status := "unavailable"
			if err == nil {
				switch {
				case st.Ready:
					status = "available"
				case strings.HasPrefix(st.HelmStatus, "pending"):
					status = "starting"
				}
			} else {
				log.Printf("home: status for %s: %v", e.Name, err)
			}
			vpn := false
			for _, route := range e.EffectiveRoutes() {
				if route.Mode == "site" && route.DefaultExposure() == "vpn" {
					vpn = true
					break
				}
			}
			out[i].Status = status
			out[i].VPN = vpn
		}()
	}
	wg.Wait()
	return out
}

// newHTTPServer sets the timeouts explicitly. Without them a slow or stuck
// client can hold connections indefinitely, and a stalled upstream can keep a
// handler alive forever.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// External storage apply runs Helm with a 5-minute wait and may follow
		// a slow showmount probe. Keep the admin response open long enough to
		// return the real result instead of a truncated timeout.
		WriteTimeout:   10 * time.Minute,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// versionHandler serves the running version and the cached release check. The
// check runs in the background, so this never waits on the network: it returns
// the last answer and the UI shows nothing while it is unknown.
func versionHandler(updates *update.Checker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, updates.Status())
	}
}

// updateAvailable reports whether the catalog offers a newer chart than the
// installed release. The comparison is by version, not by string: an install
// that is ahead of the catalog (a candidate, or a newer chart kept from an
// earlier catalog) must not be offered a downgrade.
func updateAvailable(installed, catalogued string) bool {
	if installed == "" || catalogued == "" {
		return false
	}
	return update.Compare(installed, catalogued) < 0
}

// appStatus is one row of the Applications section.
type appStatus struct {
	app.Status
	Bootstrap       string `json:"bootstrap,omitempty"`
	LatestChart     string `json:"latestChart,omitempty"`
	LatestApp       string `json:"latestAppVersion,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
}

// mutating rejects a state-changing request from another origin. The signup
// redemption is deliberately open: it is token-authenticated and posted by the
// identity chart's nginx, not by a browser session.
func mutating(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

// tokenPrefix identifies an invitation in the audit log without recording a
// usable credential.
func tokenPrefix(token string) string {
	if len(token) <= 8 {
		return "[short]"
	}
	return token[:8]
}

// sameOrigin rejects a state-changing request that a browser sends from another
// origin. The session cookie is SameSite=Lax, which already covers most of it;
// this closes the JSON-fetch hole and costs nothing.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // curl, the CLI, same-origin form posts without Origin
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// audit records who did a privileged thing. Never log values, only identities
// and the action.
func audit(r *http.Request, auth *consoleAuth, action, name, detail string) {
	user, _ := auth.currentUser(r)
	log.Printf("audit user=%q action=%s app=%q %s", user, action, name, detail)
}
