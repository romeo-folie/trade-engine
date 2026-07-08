package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestConfigExample_InSyncWithStruct is the drift guard for config.example.yaml.
// It fails when:
//
//  1. the example contains a key the Config struct no longer knows
//     (strict decode — catches renamed/removed options lingering in the doc), or
//  2. the Config struct has a yaml-tagged field path the example never mentions
//     (completeness walk — catches new options never documented).
//
// The example is the only user-facing catalogue of every knob; before this test
// it had silently lost six entire top-level sections (scanner, feedback,
// forex_orb, futures_orb, futures_orb_metals, portfolio).
func TestConfigExample_InSyncWithStruct(t *testing.T) {
	raw, err := os.ReadFile(examplePath(t))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}

	// (1) Strict decode: unknown keys in the example fail.
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("config.example.yaml contains keys the Config struct does not know (stale/renamed option?): %v", err)
	}

	// (2) Completeness: every yaml-tagged struct path must appear in the example.
	structPaths := map[string]struct{}{}
	collectStructPaths(reflect.TypeOf(Config{}), "", structPaths)

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse example yaml: %v", err)
	}
	examplePaths := map[string]struct{}{}
	collectYAMLPaths(&doc, "", examplePaths)

	// Deliberately undocumented paths. The legacy single-model / inline-ensemble
	// fields must stay COMMENTED in the example (live values would activate
	// legacy mode); cost.cache_ttl_min is profile-owned and documented as a
	// commented override so an explicit value doesn't silently pin the profile.
	exempt := []string{
		"models.analysts",
		"models.arbiter",
		"models.primary",
		"models.secondary",
		"models.consensus.strategy",
		"cost.cache_ttl_min",
	}
	isExempt := func(path string) bool {
		for _, e := range exempt {
			if path == e || strings.HasPrefix(path, e+".") {
				return true
			}
		}
		return false
	}

	var missing []string
	for p := range structPaths {
		if _, ok := examplePaths[p]; !ok && !isExempt(p) {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("config.example.yaml is missing %d documented option(s):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

func examplePath(t *testing.T) string {
	t.Helper()
	// Test runs from internal/config; the example sits at the repo root.
	p, err := filepath.Abs(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("resolve example path: %v", err)
	}
	return p
}

// collectStructPaths walks yaml-tagged exported fields, recursing into struct
// values (including slices/maps OF structs is deliberately skipped: their keys
// are data, not schema).
func collectStructPaths(tt reflect.Type, prefix string, out map[string]struct{}) {
	if tt.Kind() == reflect.Ptr {
		tt = tt.Elem()
	}
	if tt.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < tt.NumField(); i++ {
		f := tt.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		path := tag
		if prefix != "" {
			path = prefix + "." + tag
		}
		out[path] = struct{}{}
		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft.PkgPath() != "time" {
			collectStructPaths(ft, path, out)
		}
	}
}

// collectYAMLPaths records every mapping-key path present in the yaml document.
func collectYAMLPaths(n *yaml.Node, prefix string, out map[string]struct{}) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			collectYAMLPaths(c, prefix, out)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			path := key.Value
			if prefix != "" {
				path = prefix + "." + key.Value
			}
			out[path] = struct{}{}
			collectYAMLPaths(val, path, out)
		}
	}
}

var _ = fmt.Sprintf // keep fmt for debugging convenience
