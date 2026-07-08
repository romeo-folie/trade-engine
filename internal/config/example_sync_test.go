package config

import (
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
// The walk recurses into struct-typed slice elements ("[]" segment) and map
// values ("*" segment): a field like Instrument.margin_rate is satisfied when
// at least one documented instrument shows it, and ModelConfig.reasoning_effort
// when at least one registry entry does — otherwise per-element schema could
// drift invisibly (Codex #394).
//
// The example is the only user-facing catalogue of every knob; before this test
// it had silently lost 135 options across six entire top-level sections.
func TestConfigExample_InSyncWithStruct(t *testing.T) {
	raw, err := os.ReadFile(examplePath(t))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}

	// (1) Strict decode: unknown keys anywhere in the example fail.
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("config.example.yaml contains keys the Config struct does not know (stale/renamed option?): %v", err)
	}

	// (2) Completeness: every yaml-tagged struct path must appear in the example.
	structPaths := map[string]struct{}{}
	collectStructPaths(reflect.TypeOf(Config{}), nil, structPaths)

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse example yaml: %v", err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}

	// Deliberately undocumented paths. The legacy single-model / inline-ensemble
	// fields must stay COMMENTED in the example (live values would activate
	// legacy mode); cost.cache_ttl_min is profile-owned and documented as a
	// commented override so an explicit value doesn't silently pin the profile;
	// model api_key inlines a raw secret (api_key_env is the documented form).
	exempt := []string{
		"models.analysts",
		"models.arbiter",
		"models.primary",
		"models.secondary",
		"models.consensus.strategy",
		"models.registry.*.api_key",
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
		if isExempt(p) {
			continue
		}
		if !yamlHasPath(root, strings.Split(p, ".")) {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("config.example.yaml is missing %d documented option(s) ('[]' = any list element, '*' = any map entry):\n  %s",
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
// values, struct-typed slice elements (as a "[]" segment) and struct-typed map
// values (as a "*" segment).
func collectStructPaths(tt reflect.Type, prefix []string, out map[string]struct{}) {
	if tt.Kind() == reflect.Pointer {
		tt = tt.Elem()
	}
	if tt.Kind() != reflect.Struct || tt.PkgPath() == "time" {
		return
	}
	for i := 0; i < tt.NumField(); i++ {
		f := tt.Field(i)
		if !f.IsExported() {
			continue
		}
		parts := strings.Split(f.Tag.Get("yaml"), ",")
		tag := parts[0]
		// yaml:",inline" embeds the struct's fields at the SAME level — walk them
		// under the current prefix. Skipping these hid the entire embedded
		// ModelConfig inside ModelRegistryEntry (Codex #394).
		if tag == "" && len(parts) > 1 && contains(parts[1:], "inline") {
			collectStructPaths(deref(f.Type), prefix, out)
			continue
		}
		if tag == "" || tag == "-" {
			continue
		}
		path := append(append([]string{}, prefix...), tag)
		out[strings.Join(path, ".")] = struct{}{}

		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch ft.Kind() {
		case reflect.Struct:
			collectStructPaths(ft, path, out)
		case reflect.Slice, reflect.Array:
			if elem := deref(ft.Elem()); elem.Kind() == reflect.Struct {
				collectStructPaths(elem, append(path, "[]"), out)
			}
		case reflect.Map:
			if elem := deref(ft.Elem()); elem.Kind() == reflect.Struct {
				collectStructPaths(elem, append(path, "*"), out)
			}
		}
	}
}

func deref(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Pointer {
		return t.Elem()
	}
	return t
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// yamlHasPath reports whether the document contains a node at the given path.
// "[]" matches ANY element of a sequence and "*" ANY value of a mapping —
// union semantics: one documented instrument/registry entry showing a field
// satisfies that field for the whole collection.
func yamlHasPath(n *yaml.Node, segs []string) bool {
	if n == nil {
		return false
	}
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if len(segs) == 0 {
		return true
	}
	seg := segs[0]
	switch seg {
	case "[]":
		if n.Kind != yaml.SequenceNode {
			return false
		}
		for _, item := range n.Content {
			if yamlHasPath(item, segs[1:]) {
				return true
			}
		}
		return false
	case "*":
		if n.Kind != yaml.MappingNode {
			return false
		}
		for i := 1; i < len(n.Content); i += 2 {
			if yamlHasPath(n.Content[i], segs[1:]) {
				return true
			}
		}
		return false
	default:
		if n.Kind != yaml.MappingNode {
			return false
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == seg {
				return yamlHasPath(n.Content[i+1], segs[1:])
			}
		}
		return false
	}
}
