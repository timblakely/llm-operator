package main

// The cache manager is deliberately a sidecar, rather than a Kubernetes
// controller: the proxy serializes model switches, so it can safely own the
// only writer to the hot caches.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type cacheRequest struct {
	Model   string    `json:"model"`
	Backend string    `json:"backend"`
	Cache   cacheSpec `json:"cache"`
}

// artifactManifest is written only after a complete cold artifact has been
// staged.  The inventory covers the materialized payload rather than the HF
// cache metadata, which makes it useful for both Hugging Face and GGUF file
// artifacts and catches NAS corruption before a restore.
type artifactManifest struct {
	Version int            `json:"version"`
	Cache   cacheSpec      `json:"cache"`
	Files   []artifactFile `json:"files"`
}

type artifactFile struct {
	Path      string `json:"path"`
	Size      int64  `json:"size,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	SymlinkTo string `json:"symlink_to,omitempty"`
}

type cacheManager struct {
	hotRoot    string
	cold       string
	maxPercent float64
	logger     *slog.Logger
	opMu       sync.Mutex
	limits     map[string]int64
	sweeps     atomic.Uint64
	evictions  atomic.Uint64
	archived   atomic.Int64
	hydrated   atomic.Int64
	failures   atomic.Uint64
}

func runCacheManager() {
	// The previous four crashes (Exit Code 2, no panic trace in either the
	// crashing or the following container's logs) are consistent with a
	// panic in a detached background goroutine, which net/http's per-request
	// recovery cannot catch. "system" tracebacks dump every goroutine's stack
	// on a fatal error (not just a plain panic's), so a Go-runtime-level
	// fault - not just an application panic - leaves a diagnosable trace next
	// time instead of a silent restart.
	debug.SetTraceback("system")
	m := &cacheManager{
		hotRoot: env("CACHE_HOT_ROOT", "/cache/hot"),
		cold:    env("CACHE_COLD", "/cold"), maxPercent: 95,
		limits: map[string]int64{},
		logger: slog.New(slog.NewTextHandler(os.Stdout, nil)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ensure", m.ensure)
	mux.HandleFunc("POST /v1/sweep", m.sweepRequest)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /metrics", m.metrics)
	addr := env("CACHE_MANAGER_LISTEN", ":8090")
	if err := m.removeStaging(); err != nil {
		m.logger.Warn("remove incomplete cache staging", "error", err)
	}
	m.discoverPVCLimits()
	m.logger.Info("starting cache manager", "address", addr, "cold", m.cold)
	if err := http.ListenAndServe(addr, mux); err != nil {
		m.logger.Error("cache manager stopped", "error", err)
		os.Exit(1)
	}
}

func (m *cacheManager) discoverPVCLimits() {
	config, err := rest.InClusterConfig()
	if err != nil {
		m.logger.Warn("discover PVC limits", "error", err)
		return
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		m.logger.Warn("create Kubernetes client", "error", err)
		return
	}
	podName, err := os.Hostname()
	if err != nil {
		return
	}
	namespace := env("POD_NAMESPACE", mustNamespace())
	pod, err := client.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		m.logger.Warn("get cache-manager Pod", "error", err)
		return
	}
	var mounts []corev1.VolumeMount
	for _, container := range pod.Spec.Containers {
		if container.Name == "cache-manager" {
			mounts = container.VolumeMounts
			break
		}
	}
	for _, mount := range mounts {
		if mount.MountPath != m.hotRoot {
			continue
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.Name != mount.Name || volume.PersistentVolumeClaim == nil {
				continue
			}
			pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(context.Background(), volume.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
			if err == nil && pvc.Status.Capacity.Storage() != nil {
				m.limits[mount.MountPath] = pvc.Status.Capacity.Storage().Value()
			}
		}
	}
	m.logger.Info("discovered shared hot PVC limit", "hot_root", m.hotRoot, "capacity_bytes", m.limits[m.hotRoot])
}

func (m *cacheManager) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	for _, cache := range []struct {
		name string
		path string
	}{{"hot", m.hotRoot}, {"cold", m.cold}} {
		capacity, used, err := m.cacheUsage(cache.path)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "llm_cache_manager_filesystem_bytes{cache=%q,state=\"capacity\"} %d\n", cache.name, capacity)
		fmt.Fprintf(w, "llm_cache_manager_filesystem_bytes{cache=%q,state=\"used\"} %d\n", cache.name, used)
	}
	fmt.Fprintf(w, "llm_cache_manager_sweeps_total %d\n", m.sweeps.Load())
	fmt.Fprintf(w, "llm_cache_manager_evictions_total %d\n", m.evictions.Load())
	fmt.Fprintf(w, "llm_cache_manager_archived_bytes_total %d\n", m.archived.Load())
	fmt.Fprintf(w, "llm_cache_manager_hydrated_bytes_total %d\n", m.hydrated.Load())
	fmt.Fprintf(w, "llm_cache_manager_failures_total %d\n", m.failures.Load())
}

func (m *cacheManager) sweepRequest(w http.ResponseWriter, r *http.Request) {
	var request cacheRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validCacheRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.logger.Info("sweep request received", "model", request.Model, "backend", request.Backend)
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.sweep(m.hotRoot, cacheKey(request.Cache)); err != nil {
		m.failures.Add(1)
		m.logger.Error("sweep failed", "model", request.Model, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m.logger.Info("sweep request completed", "model", request.Model)
	w.WriteHeader(http.StatusNoContent)
}

func (m *cacheManager) ensure(w http.ResponseWriter, r *http.Request) {
	var request cacheRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validCacheRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.logger.Info("ensure request received", "model", request.Model, "backend", request.Backend, "repo", request.Cache.RepoID)
	m.opMu.Lock()
	defer m.opMu.Unlock()
	result := m.cacheResult(request)
	m.logger.Info("cache state evaluated", "model", request.Model, "state", result)
	start := time.Now()
	if err := m.ensureArtifact(request); err != nil {
		m.failures.Add(1)
		m.logger.Error("ensure artifact failed", "model", request.Model, "elapsed", time.Since(start), "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m.logger.Info("ensure request completed", "model", request.Model, "state", result, "elapsed", time.Since(start))
	w.Header().Set("X-LLM-Cache-Result", result)
	w.WriteHeader(http.StatusNoContent)
}

func (m *cacheManager) removeStaging() error {
	// A cache-manager restart can interrupt a Hugging Face download.  Neither
	// cold nor hot staging directories contain a complete artifact, so remove
	// them before accepting another ensure request.  Keep the legacy hot-root
	// pattern as well: previous releases placed the temporary directory beside
	// (rather than within) hotRoot/staging.
	patterns := []string{
		filepath.Join(m.cold, "artifacts", "*.staging-*"),
		filepath.Join(m.hotRoot, "staging", "*.staging-*"),
		filepath.Join(m.hotRoot, "staging.staging-*"),
	}
	for _, pattern := range patterns {
		entries, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			m.logger.Info("cleaning up stale staging directory", "path", entry)
			if err := os.RemoveAll(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *cacheManager) cacheResult(r cacheRequest) string {
	if complete(filepath.Join(m.hotRoot, ".llm-cache", cacheKey(r.Cache))) {
		return "hot"
	}
	if complete(filepath.Join(m.cold, "artifacts", cacheKey(r.Cache))) {
		return "cold"
	}
	if m.hotArtifactExists(r, m.hotRoot) {
		return "hot"
	}
	return "external"
}

func validCacheRequest(r cacheRequest) error {
	if r.Model == "" || (r.Backend != "vllm" && r.Backend != "llama-cpp") {
		return errors.New("model and supported backend are required")
	}
	if (r.Cache.Kind != "huggingface-hub" && r.Cache.Kind != "huggingface-files") || r.Cache.RepoID == "" || r.Cache.Revision == "" || r.Cache.Size < 0 {
		return errors.New("invalid immutable cache specification")
	}
	if r.Cache.Kind == "huggingface-files" && (len(r.Cache.Files) == 0 || r.Cache.Size < 1) {
		return errors.New("file cache requires files")
	}
	if r.Cache.Kind == "huggingface-files" {
		if err := validMaterializationTarget(r.Cache.MaterializationTarget); err != nil {
			return err
		}
	} else if r.Cache.MaterializationTarget != "" {
		return errors.New("hub cache must not set materialization_target")
	}
	return nil
}

// validMaterializationTarget confines file artifacts to an immutable directory
// below gguf. The target is supplied by the operator, so it is treated as an
// untrusted path even though the manager is the only hot-cache writer.
func validMaterializationTarget(target string) error {
	clean := filepath.ToSlash(filepath.Clean(target))
	if target == "" || filepath.IsAbs(target) || clean != target || target == "gguf" || !strings.HasPrefix(target, "gguf/") {
		return errors.New("file cache requires a clean materialization_target below gguf/")
	}
	return nil
}

func materializedPath(hot string, spec cacheSpec) string {
	if spec.Kind == "huggingface-hub" {
		return filepath.Join(hot, "hub", "models--"+strings.ReplaceAll(spec.RepoID, "/", "--"))
	}
	return filepath.Join(hot, filepath.FromSlash(spec.MaterializationTarget))
}

// resolveCacheSpec keeps desired model configuration small: a HF hub model
// needs only its repo and immutable revision. The exact file list and byte
// total are discovered only on the first cold operation and persisted in the
// artifact manifest thereafter.
func (m *cacheManager) resolveCacheSpec(spec *cacheSpec) error {
	if spec.Kind != "huggingface-hub" || spec.Size > 0 {
		return nil
	}
	m.logger.Info("discovering HuggingFace repository inventory", "repo", spec.RepoID, "revision", spec.Revision)
	script := `import json; from huggingface_hub import HfApi; files=[]; total=0
for x in HfApi().list_repo_tree("` + spec.RepoID + `", revision="` + spec.Revision + `", recursive=True, expand=True):
  path=getattr(x,"path",None); size=getattr(x,"size",None)
  if path and size is not None: files.append(path); total += size
print(json.dumps({"size":total,"files":files}))`
	output, err := exec.Command("python", "-c", script).Output()
	if err != nil {
		return fmt.Errorf("discover %s@%s: %w", spec.RepoID, spec.Revision, err)
	}
	var discovered struct {
		Size  int64    `json:"size"`
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(output, &discovered); err != nil || discovered.Size < 1 || len(discovered.Files) == 0 {
		return fmt.Errorf("discover %s: invalid inventory", spec.RepoID)
	}
	sort.Strings(discovered.Files)
	spec.Size, spec.Files = discovered.Size, discovered.Files
	m.logger.Info("discovered HuggingFace inventory", "repo", spec.RepoID, "size_bytes", spec.Size, "file_count", len(spec.Files))
	return nil
}

// runRecovered runs fn in a new goroutine. Unlike an HTTP handler - where
// net/http's own per-request recover() turns a panic into a closed
// connection rather than a crash - a detached background goroutine like the
// cold-archive copy below has nothing catching a panic above it: left
// unrecovered, it takes down the whole process, silently failing every
// other in-flight and future request along with it. Convert that into a
// logged error with a full stack trace instead.
func (m *cacheManager) runRecovered(op, key string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				m.failures.Add(1)
				m.logger.Error("panic in background cache operation", "op", op, "key", key, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}

func (m *cacheManager) ensureArtifact(r cacheRequest) error {
	hot := m.hotRoot
	key := cacheKey(r.Cache)
	coldArtifact := filepath.Join(m.cold, "artifacts", key)
	hotArtifact := filepath.Join(hot, ".llm-cache", key)
	if complete(hotArtifact) {
		// A completed artifact records that its bytes are hot, not that a
		// particular materialization layout is still valid.  In particular,
		// older releases could leave a nested GGUF snapshot link pointing
		// outside the gguf subPath mounted by llama.cpp. Repair that small link
		// tree from the retained hot blobs rather than treating it as a cache
		// miss and downloading the model again.
		if !m.hotArtifactExists(r, hot) {
			if err := m.repairHotMaterialization(r, hot); err != nil {
				return fmt.Errorf("repair hot materialization: %w", err)
			}
		}
		m.logger.Info("model artifact already complete in hot cache", "key", key)
		if !validArtifactMetadata(coldArtifact, r.Cache) {
			m.logger.Info("refreshing cold artifact manifest from hot cache", "key", key)
			m.runRecovered("archive hot", key, func() {
				if err := m.archiveHot(r, hot, coldArtifact); err != nil {
					m.logger.Error("background archive hot failed", "key", key, "error", err)
				}
			})
		}
		_ = os.Chtimes(hotArtifact, time.Now(), time.Now())
		return m.sweep(hot, key)
	}
	if err := m.resolveCacheSpec(&r.Cache); err != nil {
		return err
	}
	if err := m.sweep(hot, ""); err != nil {
		return err
	}
	if err := m.makeSpace(hot, r.Cache.Size); err != nil {
		return err
	}

	manifest, ok := artifactManifestFor(coldArtifact, r.Cache)
	if ok {
		m.logger.Info("cold artifact hit, promoting from cold NAS to hot NVMe", "key", key, "files", len(manifest.Files))
		if err := m.clearMaterialized(r, hot); err != nil {
			return err
		}
		if err := m.materialize(r, coldArtifact, hot, manifest); err != nil {
			return fmt.Errorf("promote from cold: %w", err)
		}
	} else {
		m.logger.Info("downloading model directly from HuggingFace to hot NVMe", "key", key)
		if err := m.clearMaterialized(r, hot); err != nil {
			return err
		}
		if err := m.downloadToHot(r, hot); err != nil {
			return fmt.Errorf("download to hot: %w", err)
		}
	}

	if err := os.MkdirAll(hotArtifact, 0o755); err != nil {
		return err
	}
	if err := writeComplete(hotArtifact); err != nil {
		return err
	}
	metadata, _ := json.Marshal(r.Cache)
	if err := os.WriteFile(filepath.Join(hotArtifact, "cache.json"), metadata, 0o644); err != nil {
		return err
	}

	m.logger.Info("successfully ensured hot artifact", "key", key)

	// Asynchronously write to cold storage so hot serving is ready immediately
	m.runRecovered("cold archive", key, func() {
		m.logger.Info("archiving hot artifact to cold storage in background", "key", key)
		if err := m.archiveHot(r, hot, coldArtifact); err != nil {
			m.logger.Error("background cold archiving failed", "key", key, "error", err)
		} else {
			m.logger.Info("background cold archiving completed", "key", key)
		}
	})

	return nil
}

// repairHotMaterialization repairs the GGUF link tree in place. Earlier
// releases kept blobs at hot/blobs and nested links escaped the gguf subPath
// mounted at /models. Move that existing blob store below gguf first, then
// atomically relink the requested artifact. This deliberately never fetches
// or copies model bytes.
func (m *cacheManager) repairHotMaterialization(r cacheRequest, hot string) error {
	if r.Cache.Kind != "huggingface-files" {
		return errors.New("cannot repair invalid non-file hot materialization")
	}
	legacyBlobs := filepath.Join(hot, "blobs")
	hotBlobs := filepath.Join(hot, "gguf", ".blobs")
	if _, err := os.Lstat(hotBlobs); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(hotBlobs), 0o755); err != nil {
			return err
		}
		if err := os.Rename(legacyBlobs, hotBlobs); err != nil {
			return fmt.Errorf("move legacy blob store: %w", err)
		}
		// Preserve the old host-root path for any other hot artifact that has
		// not yet been re-linked. It remains intentionally unreachable from a
		// gguf subPath mount, while the corrected links stay within that mount.
		if err := os.Symlink(filepath.Join("gguf", ".blobs"), legacyBlobs); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	for _, file := range r.Cache.Files {
		path := filepath.Join(materializedPath(hot, r.Cache), file)
		oldTarget, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("read legacy link %s: %w", file, err)
		}
		blob := filepath.Join(hotBlobs, filepath.Base(oldTarget))
		info, err := os.Stat(blob)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("resolve retained blob for %s", file)
		}
		target, err := relativeBlobLink(path, blob)
		if err != nil {
			return err
		}
		if err := replaceSymlink(path, target); err != nil {
			return err
		}
	}
	return nil
}

func (m *cacheManager) downloadToHot(r cacheRequest, hot string) error {
	staging, err := stagingDir(filepath.Join(hot, "staging"))
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	args := []string{"download", r.Cache.RepoID, "--revision", r.Cache.Revision, "--cache-dir", filepath.Join(staging, "hub")}
	args = append(args, r.Cache.Files...)
	command := exec.Command("hf", args...)
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("download %s: %w: %s", r.Cache.RepoID, err, strings.TrimSpace(string(output)))
	}

	if r.Cache.Kind == "huggingface-hub" {
		source := filepath.Join(staging, "hub", "models--"+strings.ReplaceAll(r.Cache.RepoID, "/", "--"))
		dest := materializedPath(hot, r.Cache)
		if err := copyTree(source, dest); err != nil {
			return err
		}
	} else {
		for _, file := range r.Cache.Files {
			source := filepath.Join(staging, "hub", "models--"+strings.ReplaceAll(r.Cache.RepoID, "/", "--"), "snapshots", r.Cache.Revision, file)
			dest := filepath.Join(materializedPath(hot, r.Cache), file)
			// llama.cpp mounts only hot/gguf at /models. Keep the shared blob
			// store below that mount so file symlinks remain resolvable to the
			// inference container without duplicating blobs per model.
			if err := copyHuggingFaceFile(source, dest, filepath.Join(hot, "gguf", ".blobs")); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *cacheManager) clearMaterialized(r cacheRequest, hot string) error {
	return os.RemoveAll(materializedPath(hot, r.Cache))
}

func (m *cacheManager) hotArtifactExists(r cacheRequest, hot string) bool {
	if r.Cache.Kind == "huggingface-hub" {
		_, err := os.Stat(materializedPath(hot, r.Cache))
		return err == nil
	}
	for _, file := range r.Cache.Files {
		path := filepath.Join(materializedPath(hot, r.Cache), file)
		if _, err := os.Stat(path); err != nil {
			return false
		}
		// llama.cpp sees hot/gguf as /models, so a host-valid link that climbs
		// above gguf is still invalid to the inference container.
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return false
		}
		relative, err := filepath.Rel(filepath.Join(hot, "gguf"), resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return false
		}
	}
	return true
}

func (m *cacheManager) archiveHot(r cacheRequest, hot, destination string) error {
	staging, err := stagingDir(destination)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if r.Cache.Kind == "huggingface-hub" {
		source := materializedPath(hot, r.Cache)
		if err := copyTree(source, filepath.Join(staging, "payload", "hub", filepath.Base(source))); err != nil {
			return err
		}
	} else {
		for _, file := range r.Cache.Files {
			if err := copyHuggingFaceFile(filepath.Join(materializedPath(hot, r.Cache), file), filepath.Join(staging, "payload", "files", file), filepath.Join(staging, "blobs")); err != nil {
				return err
			}
		}
	}
	if err := writeArtifactManifest(staging, r.Cache); err != nil {
		return err
	}
	if err := writeComplete(staging); err != nil {
		return err
	}
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		return err
	}
	m.archived.Add(r.Cache.Size)
	return nil
}

func (m *cacheManager) sweep(hot, preserve string) error {
	m.sweeps.Add(1)
	capacity, used, err := m.cacheUsage(hot)
	if err != nil || used <= capacity*80/100 {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(hot, ".llm-cache"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entryModTime(entries[i]).Before(entryModTime(entries[j]))
	})
	for _, entry := range entries {
		if entry.Name() == preserve {
			continue
		}
		if err := m.evict(hot, filepath.Join(hot, ".llm-cache", entry.Name())); err != nil {
			return err
		}
		_, used, err = m.cacheUsage(hot)
		if err != nil {
			return err
		}
		if used <= capacity*65/100 {
			return nil
		}
	}
	return nil
}

func cacheKey(c cacheSpec) string {
	return strings.NewReplacer("/", "--", "@", "-").Replace(c.Kind + "-" + c.RepoID + "-" + c.Revision)
}

// entryModTime stats a directory entry for eviction ordering. A failed stat
// - most plausibly a TOCTOU race where the entry is removed between the
// ReadDir call and this one - previously left a nil os.FileInfo that the
// caller's sort comparator dereferenced directly, panicking. Sort a
// vanished/inaccessible entry as the oldest so it sorts first rather than
// crashing the comparison.
func entryModTime(entry os.DirEntry) time.Time {
	info, err := entry.Info()
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func complete(dir string) bool { _, err := os.Stat(filepath.Join(dir, ".complete")); return err == nil }
func writeComplete(dir string) error {
	return os.WriteFile(filepath.Join(dir, ".complete"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

func (m *cacheManager) downloadToCold(r cacheRequest, destination string) error {
	staging, err := stagingDir(destination)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.MkdirAll(filepath.Join(staging, "payload"), 0o755); err != nil {
		return err
	}
	args := []string{"download", r.Cache.RepoID, "--revision", r.Cache.Revision, "--cache-dir", filepath.Join(staging, "hub")}
	args = append(args, r.Cache.Files...)
	command := exec.Command("hf", args...)
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("download %s: %w: %s", r.Cache.RepoID, err, strings.TrimSpace(string(output)))
	}
	if r.Cache.Kind == "huggingface-hub" {
		repo := filepath.Join(staging, "hub", "models--"+strings.ReplaceAll(r.Cache.RepoID, "/", "--"))
		if err := copyTree(repo, filepath.Join(staging, "payload", "hub", filepath.Base(repo))); err != nil {
			return err
		}
	} else {
		for _, file := range r.Cache.Files {
			source := filepath.Join(staging, "hub", "models--"+strings.ReplaceAll(r.Cache.RepoID, "/", "--"), "snapshots", r.Cache.Revision, file)
			if err := copyHuggingFaceFile(source, filepath.Join(staging, "payload", "files", file), filepath.Join(staging, "blobs")); err != nil {
				return err
			}
		}
	}
	if err := writeArtifactManifest(staging, r.Cache); err != nil {
		return err
	}
	if err := writeComplete(staging); err != nil {
		return err
	}
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	return os.Rename(staging, destination)
}

func stagingDir(destination string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(filepath.Dir(destination), filepath.Base(destination)+".staging-")
}

func writeArtifactManifest(dir string, spec cacheSpec) error {
	files, err := artifactInventory(filepath.Join(dir, "payload"))
	if err != nil {
		return err
	}
	body, err := json.Marshal(artifactManifest{Version: 1, Cache: spec, Files: files})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "artifact.json"), body, 0o644)
}

func validArtifact(dir string, want cacheSpec) bool {
	if !complete(dir) {
		return false
	}
	body, err := os.ReadFile(filepath.Join(dir, "artifact.json"))
	if err != nil {
		return false
	}
	var got artifactManifest
	if json.Unmarshal(body, &got) != nil {
		return false
	}
	if !validArtifactManifest(got, want) {
		return false
	}
	files, err := artifactInventory(filepath.Join(dir, "payload"))
	return err == nil && slices.Equal(got.Files, files)
}

func validArtifactMetadata(dir string, want cacheSpec) bool {
	_, ok := artifactManifestFor(dir, want)
	return ok
}

func artifactManifestFor(dir string, want cacheSpec) (artifactManifest, bool) {
	if !complete(dir) {
		return artifactManifest{}, false
	}
	body, err := os.ReadFile(filepath.Join(dir, "artifact.json"))
	if err != nil {
		return artifactManifest{}, false
	}
	var got artifactManifest
	return got, json.Unmarshal(body, &got) == nil && validArtifactManifest(got, want)
}

func validArtifactManifest(got artifactManifest, want cacheSpec) bool {
	return got.Version == 1 && sameCacheSpec(got.Cache, want) && len(got.Files) > 0
}

func sameCacheSpec(got, want cacheSpec) bool {
	return got.Kind == want.Kind && got.RepoID == want.RepoID && got.Revision == want.Revision && (want.Size == 0 || got.Size == want.Size) && (len(want.Files) == 0 || slices.Equal(got.Files, want.Files))
}

var ioBufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 1024*1024) // 1MB reusable buffer
		return &buf
	},
}

func artifactInventory(root string) ([]artifactFile, error) {
	var files []artifactFile
	bufPtr := ioBufferPool.Get().(*[]byte)
	defer ioBufferPool.Put(bufPtr)
	buf := *bufPtr

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root || entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		file := artifactFile{Path: filepath.ToSlash(relative)}
		if info.Mode()&os.ModeSymlink != 0 {
			file.SymlinkTo, err = os.Readlink(path)
			if err != nil {
				return err
			}
			files = append(files, file)
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported artifact file %s", relative)
		}
		handle, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.CopyBuffer(hash, handle, buf)
		closeErr := handle.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		file.Size = info.Size()
		file.SHA256 = fmt.Sprintf("%x", hash.Sum(nil))
		files = append(files, file)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func (m *cacheManager) materialize(r cacheRequest, artifact, hot string, manifest artifactManifest) error {
	for _, file := range manifest.Files {
		relative := file.Path
		if r.Cache.Kind == "huggingface-files" {
			var ok bool
			relative, ok = strings.CutPrefix(relative, "files/")
			if !ok || relative == "" {
				return fmt.Errorf("invalid file artifact path %s", file.Path)
			}
			relative = filepath.ToSlash(filepath.Join(r.Cache.MaterializationTarget, relative))
		}
		from := filepath.Join(artifact, "payload", filepath.FromSlash(file.Path))
		to := filepath.Join(hot, filepath.FromSlash(relative))
		if file.SymlinkTo != "" {
			target, err := os.Readlink(from)
			if err != nil || target != file.SymlinkTo {
				return fmt.Errorf("verify symlink %s", file.Path)
			}
			// Hugging Face snapshots link files into their local blob store.  A
			// materialized model lives at a different depth, so preserve the blob
			// once under the shared hot root and derive a new relative link from
			// its final location.
			sourceBlob := filepath.Clean(filepath.Join(filepath.Dir(from), target))
			artifactRelative, err := filepath.Rel(artifact, sourceBlob)
			if err != nil || artifactRelative == ".." || strings.HasPrefix(artifactRelative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("invalid symlink target for %s", file.Path)
			}
			info, err := os.Stat(sourceBlob)
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("resolve symlink %s", file.Path)
			}
			// File-backed models are exposed through hot/gguf (mounted as
			// /models).  Keeping blobs under gguf/.blobs ensures the rewritten
			// relative links resolve both from the cache-manager and llama.cpp.
			hotBlob := filepath.Join(hot, "gguf", ".blobs", filepath.Base(sourceBlob))
			if _, err := os.Lstat(hotBlob); os.IsNotExist(err) {
				if err := copyAndVerify(sourceBlob, hotBlob, artifactFile{Path: file.Path, Size: info.Size()}); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
				return err
			}
			hotTarget, err := relativeBlobLink(to, hotBlob)
			if err != nil {
				return err
			}
			if err := replaceSymlink(to, hotTarget); err != nil {
				return err
			}
			continue
		}
		if err := copyAndVerify(from, to, file); err != nil {
			return err
		}
	}
	m.hydrated.Add(r.Cache.Size)
	return nil
}

func copyAndVerify(source, destination string, want artifactFile) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	out, err := os.Create(destination)
	if err != nil {
		return err
	}

	bufPtr := ioBufferPool.Get().(*[]byte)
	defer ioBufferPool.Put(bufPtr)
	buf := *bufPtr

	var writer io.Writer = out
	var hash hash.Hash
	if want.SHA256 != "" {
		hash = sha256.New()
		writer = io.MultiWriter(out, hash)
	}

	n, copyErr := io.CopyBuffer(writer, in, buf)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != want.Size || (hash != nil && fmt.Sprintf("%x", hash.Sum(nil)) != want.SHA256) {
		_ = os.Remove(destination)
		return fmt.Errorf("checksum mismatch for %s", want.Path)
	}
	return nil
}

func (m *cacheManager) makeSpace(hot string, incoming int64) error {
	capacity, used, err := m.cacheUsage(hot)
	if err != nil {
		return err
	}
	if incoming > capacity*95/100 {
		return fmt.Errorf("artifact is larger than 95%% of hot cache")
	}
	if used+incoming <= capacity*95/100 {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(hot, ".llm-cache"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// The cache-manager only runs while the proxy has scaled inference down;
	// at this point every hot artifact is safe to evict. Sort by marker mtime.
	sort.Slice(entries, func(i, j int) bool {
		return entryModTime(entries[i]).Before(entryModTime(entries[j]))
	})
	for _, entry := range entries {
		if err := m.evict(hot, filepath.Join(hot, ".llm-cache", entry.Name())); err != nil {
			return err
		}
		_, used, err = m.cacheUsage(hot)
		if err != nil {
			return err
		}
		if used+incoming <= capacity*95/100 {
			return nil
		}
	}
	return fmt.Errorf("insufficient hot-cache capacity")
}

func filesystemUsage(path string) (capacity, used int64, err error) {
	var stat syscall.Statfs_t
	if err = syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	return int64(stat.Blocks) * int64(stat.Bsize), int64(stat.Blocks-stat.Bavail) * int64(stat.Bsize), nil
}

func (m *cacheManager) cacheUsage(path string) (int64, int64, error) {
	capacity, used, err := filesystemUsage(path)
	if err != nil {
		return 0, 0, err
	}
	if limit := m.limits[path]; limit > 0 {
		// OpenEBS hostpath volumes share their backing filesystem with the
		// node.  Its statfs usage includes unrelated volumes, so pairing that
		// value with this PVC's capacity can reject a cache restore even when
		// this PVC itself is empty. Count the mounted cache tree instead.
		used, err = directoryUsage(path)
		if err != nil {
			return 0, 0, err
		}
		capacity = limit
	}
	return capacity, used, err
}

func directoryUsage(path string) (int64, error) {
	var used int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			used += info.Size()
		}
		return nil
	})
	return used, err
}

func (m *cacheManager) evict(hot, marker string) error {
	// The manifest key maps to exactly one immutable artifact. Remove its serving
	// copy and marker, but never cold data or another artifact's target.
	var spec cacheSpec
	if body, err := os.ReadFile(filepath.Join(marker, "cache.json")); err == nil {
		_ = json.Unmarshal(body, &spec)
	}
	if spec.Kind == "huggingface-hub" && spec.RepoID != "" {
		_ = os.RemoveAll(materializedPath(hot, spec))
	} else if spec.Kind == "huggingface-files" {
		_ = os.RemoveAll(materializedPath(hot, spec))
	}
	if err := os.RemoveAll(marker); err != nil {
		return err
	}
	m.evictions.Add(1)
	return nil
}

func copyTree(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(target, destination)
	}
	if !info.IsDir() {
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		in, err := os.Open(source)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(destination)
		if err != nil {
			return err
		}
		bufPtr := ioBufferPool.Get().(*[]byte)
		defer ioBufferPool.Put(bufPtr)
		buf := *bufPtr
		_, err = io.CopyBuffer(out, in, buf)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyTree(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// copyHuggingFaceFile preserves Hugging Face's deduplicated blob layout while
// rewriting a snapshot symlink for its new destination. This makes both flat
// and nested GGUF paths resolve through the destination's shared blobs root.
func copyHuggingFaceFile(source, destination, blobs string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return copyTree(source, destination)
	}
	target, err := os.Readlink(source)
	if err != nil {
		return err
	}
	sourceBlob := filepath.Clean(filepath.Join(filepath.Dir(source), target))
	blobInfo, err := os.Stat(sourceBlob)
	if err != nil || !blobInfo.Mode().IsRegular() {
		return fmt.Errorf("resolve Hugging Face blob %s", source)
	}
	destinationBlob := filepath.Join(blobs, filepath.Base(sourceBlob))
	if err := copyTree(sourceBlob, destinationBlob); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
		return err
	}
	link, err := relativeBlobLink(destination, destinationBlob)
	if err != nil {
		return err
	}
	return replaceSymlink(destination, link)
}

// relativeBlobLink computes the symlink target from the linked file's parent,
// rather than assuming a fixed snapshot depth. File-backed model repositories
// commonly nest quantization files below one or more directories.
func relativeBlobLink(linkPath, blobPath string) (string, error) {
	return filepath.Rel(filepath.Dir(linkPath), blobPath)
}

func replaceSymlink(path, target string) error {
	dir := filepath.Dir(path)
	tmp, err := os.MkdirTemp(dir, ".relink-")
	if err != nil {
		return err
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
