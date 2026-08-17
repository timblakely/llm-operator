package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoveStagingRemovesColdAndHotIncompleteDirectories(t *testing.T) {
	hot, cold := t.TempDir(), t.TempDir()
	paths := []string{
		filepath.Join(cold, "artifacts", "cold.staging-123"),
		filepath.Join(hot, "staging", "download.staging-456"),
		filepath.Join(hot, "staging.staging-789"), // pre-shared-cache layout
	}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "partial"), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	complete := filepath.Join(hot, "gguf", "model")
	if err := os.MkdirAll(complete, 0o755); err != nil {
		t.Fatal(err)
	}

	m := &cacheManager{hotRoot: hot, cold: cold, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := m.removeStaging(); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("staging directory %q still exists: %v", path, err)
		}
	}
	if _, err := os.Stat(complete); err != nil {
		t.Fatalf("complete artifact was removed: %v", err)
	}
}

func TestCacheUsageUsesMountedTreeWithPVCLimit(t *testing.T) {
	hot := t.TempDir()
	if err := os.WriteFile(filepath.Join(hot, "artifact"), make([]byte, 17), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &cacheManager{limits: map[string]int64{hot: 100}}
	capacity, used, err := m.cacheUsage(hot)
	if err != nil {
		t.Fatal(err)
	}
	if capacity != 100 || used != 17 {
		t.Fatalf("cacheUsage() = (%d, %d), want (100, 17)", capacity, used)
	}
}

func TestDirectoryUsageDoesNotCountSymlinkTarget(t *testing.T) {
	hot := t.TempDir()
	if err := os.WriteFile(filepath.Join(hot, "blob"), make([]byte, 17), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("blob", filepath.Join(hot, "snapshot")); err != nil {
		t.Fatal(err)
	}
	used, err := directoryUsage(hot)
	if err != nil {
		t.Fatal(err)
	}
	if used != 17 {
		t.Fatalf("directoryUsage() = %d, want 17", used)
	}
}

func TestMaterializeFileArtifactUsesRequestedTarget(t *testing.T) {
	artifact, hot := t.TempDir(), t.TempDir()
	payload := filepath.Join(artifact, "payload", "files", "model.gguf")
	if err := os.MkdirAll(filepath.Dir(payload), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("cold model")
	if err := os.WriteFile(payload, body, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	manifest := artifactManifest{Files: []artifactFile{{
		Path: "files/model.gguf", Size: int64(len(body)), SHA256: fmt.Sprintf("%x", digest),
	}}}
	m := &cacheManager{}
	spec := cacheSpec{Kind: "huggingface-files", MaterializationTarget: "gguf/test-model"}
	if err := m.materialize(cacheRequest{Cache: spec}, artifact, hot, manifest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(hot, "gguf", "test-model", "model.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("materialized %q, want %q", got, body)
	}
}

func TestMaterializeFileArtifactRewritesNestedBlobSymlinks(t *testing.T) {
	artifact, hot := t.TempDir(), t.TempDir()
	blob := filepath.Join(artifact, "blobs", "digest")
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("DeepSeek shard")
	if err := os.WriteFile(blob, body, 0o600); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(artifact, "payload", "files", "UD-IQ3_S", "model.gguf")
	if err := os.MkdirAll(filepath.Dir(payload), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../blobs/digest", payload); err != nil {
		t.Fatal(err)
	}
	manifest := artifactManifest{Files: []artifactFile{{Path: "files/UD-IQ3_S/model.gguf", SymlinkTo: "../../../blobs/digest"}}}
	m := &cacheManager{}
	spec := cacheSpec{Kind: "huggingface-files", MaterializationTarget: "gguf/deepseek-v4-flash"}
	if err := m.materialize(cacheRequest{Cache: spec}, artifact, hot, manifest); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(hot, "gguf", "deepseek-v4-flash", "UD-IQ3_S", "model.gguf")
	if target, err := os.Readlink(model); err != nil || target != "../../.blobs/digest" {
		t.Fatalf("materialized symlink = (%q, %v), want ../../.blobs/digest", target, err)
	}
	// Simulate llama.cpp's /models subPath mount: only hot/gguf is visible.
	modelFromMount := filepath.Join(hot, "gguf", "deepseek-v4-flash", "UD-IQ3_S", "model.gguf")
	got, err := os.ReadFile(modelFromMount)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("materialized %q, want %q", got, body)
	}
}

func TestCopyHuggingFaceFileRewritesBlobLinkForDestinationDepth(t *testing.T) {
	for _, tc := range []struct {
		name        string
		destination string
		wantLink    string
	}{
		{name: "flat", destination: "gguf/laguna/model.gguf", wantLink: "../.blobs/digest"},
		{name: "nested", destination: "gguf/deepseek-v4-flash/UD-IQ3_S/model.gguf", wantLink: "../../.blobs/digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			blob := filepath.Join(root, "hub", "models--example", "blobs", "digest")
			if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(blob, []byte("model bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(root, "hub", "models--example", "snapshots", "revision", "UD-IQ3_S", "model.gguf")
			if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../../../blobs/digest", source); err != nil {
				t.Fatal(err)
			}
			hot := filepath.Join(root, "hot")
			destination := filepath.Join(hot, tc.destination)
			if err := copyHuggingFaceFile(source, destination, filepath.Join(hot, "gguf", ".blobs")); err != nil {
				t.Fatal(err)
			}
			if got, err := os.Readlink(destination); err != nil || got != tc.wantLink {
				t.Fatalf("rewritten symlink = (%q, %v), want %q", got, err, tc.wantLink)
			}
			if got, err := os.ReadFile(destination); err != nil || string(got) != "model bytes" {
				t.Fatalf("rewritten target = (%q, %v)", got, err)
			}
		})
	}
}

func TestEnsureRepairsCompletedHotArtifactWithNestedBrokenLink(t *testing.T) {
	hot := t.TempDir()
	cache := cacheSpec{
		Kind:                  "huggingface-files",
		RepoID:                "example/deepseek",
		Revision:              "0123456789012345678901234567890123456789",
		Size:                  1,
		Files:                 []string{"UD-IQ3_S/model.gguf"},
		MaterializationTarget: "gguf/deepseek-v4-flash",
	}
	request := cacheRequest{Model: "deepseek", Backend: "llama-cpp", Cache: cache}
	key := cacheKey(cache)
	blob := filepath.Join(hot, "blobs", "digest")
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("model bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(hot, "gguf", "deepseek-v4-flash", "UD-IQ3_S", "model.gguf")
	if err := os.MkdirAll(filepath.Dir(model), 0o755); err != nil {
		t.Fatal(err)
	}
	// This is the previously emitted fixed-depth link. It escapes the gguf
	// mount and os.Stat therefore fails, while the completed hot marker remains.
	if err := os.Symlink("../../../blobs/digest", model); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(hot, ".llm-cache", key), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeComplete(filepath.Join(hot, ".llm-cache", key)); err != nil {
		t.Fatal(err)
	}

	m := &cacheManager{hotRoot: hot, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := m.ensureArtifact(request); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(model); err != nil || target != "../../.blobs/digest" {
		t.Fatalf("repaired symlink = (%q, %v), want ../../.blobs/digest", target, err)
	}
	if got, err := os.ReadFile(model); err != nil || string(got) != "model bytes" {
		t.Fatalf("repaired target = (%q, %v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(hot, "gguf", ".blobs", "digest")); err != nil {
		t.Fatalf("relocated blob store: %v", err)
	}
}

func TestFileCacheTargetValidation(t *testing.T) {
	base := cacheRequest{Model: "model", Backend: "llama-cpp", Cache: cacheSpec{
		Kind: "huggingface-files", RepoID: "example/model", Revision: "0123456789012345678901234567890123456789", Size: 1, Files: []string{"model.gguf"},
	}}
	for _, target := range []string{"", "gguf", "/gguf/model", "../gguf/model", "gguf/../model", "gguf//model"} {
		request := base
		request.Cache.MaterializationTarget = target
		if err := validCacheRequest(request); err == nil {
			t.Errorf("target %q unexpectedly accepted", target)
		}
	}
	base.Cache.MaterializationTarget = "gguf/example-model-012345"
	if err := validCacheRequest(base); err != nil {
		t.Fatalf("valid file target rejected: %v", err)
	}
	hub := base
	hub.Backend, hub.Cache.Kind, hub.Cache.MaterializationTarget = "vllm", "huggingface-hub", "gguf/not-allowed"
	if err := validCacheRequest(hub); err == nil {
		t.Fatal("hub target unexpectedly accepted")
	}
}

func TestEvictionPreservesOtherFileArtifact(t *testing.T) {
	hot := t.TempDir()
	first := cacheSpec{Kind: "huggingface-files", RepoID: "example/first", Revision: "1111111111111111111111111111111111111111", Size: 1, Files: []string{"first.gguf"}, MaterializationTarget: "gguf/first"}
	second := cacheSpec{Kind: "huggingface-files", RepoID: "example/second", Revision: "2222222222222222222222222222222222222222", Size: 1, Files: []string{"second.gguf"}, MaterializationTarget: "gguf/second"}
	for _, spec := range []cacheSpec{first, second} {
		if err := os.MkdirAll(materializedPath(hot, spec), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(materializedPath(hot, spec), spec.Files[0]), []byte("model"), 0o600); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(hot, ".llm-cache", cacheKey(spec))
		if err := os.MkdirAll(marker, 0o755); err != nil {
			t.Fatal(err)
		}
		body := []byte(fmt.Sprintf(`{"kind":%q,"repo_id":%q,"revision":%q,"size_bytes":1,"files":[%q],"materialization_target":%q}`, spec.Kind, spec.RepoID, spec.Revision, spec.Files[0], spec.MaterializationTarget))
		if err := os.WriteFile(filepath.Join(marker, "cache.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := &cacheManager{}
	if err := m.evict(hot, filepath.Join(hot, ".llm-cache", cacheKey(first))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(materializedPath(hot, first)); !os.IsNotExist(err) {
		t.Fatalf("first artifact still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(materializedPath(hot, second), second.Files[0])); err != nil {
		t.Fatalf("second artifact was removed: %v", err)
	}
}

func TestRunRecoveredConvertsPanicToLoggedErrorInsteadOfCrashing(t *testing.T) {
	var buf bytes.Buffer
	m := &cacheManager{logger: slog.New(slog.NewTextHandler(&buf, nil))}

	m.runRecovered("archive hot", "some-key", func() {
		panic("simulated background failure")
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(buf.String(), "panic in background cache operation") {
		time.Sleep(10 * time.Millisecond)
	}
	got := buf.String()
	if !strings.Contains(got, "panic in background cache operation") {
		t.Fatalf("log output = %q, want a logged panic-recovery message (the panic must not crash the process)", got)
	}
	if !strings.Contains(got, "simulated background failure") {
		t.Fatalf("log output = %q, want the panic value included", got)
	}
	if got := m.failures.Load(); got != 1 {
		t.Fatalf("failures = %d, want 1", got)
	}
}

func TestRunRecoveredRunsFnNormallyWhenNoPanic(t *testing.T) {
	var buf bytes.Buffer
	m := &cacheManager{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	done := make(chan struct{})
	m.runRecovered("op", "key", func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fn was never run")
	}
	if strings.Contains(buf.String(), "panic") {
		t.Fatalf("unexpected panic logged for a non-panicking fn: %s", buf.String())
	}
}

func TestEntryModTimeReturnsZeroForAVanishedEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir = %v, %v", entries, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// entries[0].Info() now re-stats a path that no longer exists and
	// returns an error; entryModTime must not dereference a nil FileInfo.
	if got := entryModTime(entries[0]); !got.IsZero() {
		t.Fatalf("entryModTime for a vanished entry = %v, want the zero time", got)
	}
}

func TestEntryModTimeReturnsRealModTimeForAPresentEntry(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "present"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir = %v, %v", entries, err)
	}
	if got := entryModTime(entries[0]); got.IsZero() {
		t.Fatal("entryModTime for a present file returned the zero time")
	}
}
