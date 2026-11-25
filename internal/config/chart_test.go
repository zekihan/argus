package config

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fieldPaths lists every JSON field path of t: struct fields by name, map
// values under "*", slice elements under "[]".
func fieldPaths(t reflect.Type, prefix string, out map[string]bool) {
	switch t.Kind() { //nolint:exhaustive // default covers scalar kinds.
	case reflect.Pointer, reflect.Slice:
		next := prefix
		if t.Kind() == reflect.Slice {
			next += "[]"
		}
		fieldPaths(t.Elem(), next, out)
	case reflect.Map:
		fieldPaths(t.Elem(), prefix+".*", out)
	case reflect.Struct:
		for f := range t.Fields() {
			if f.Anonymous {
				fieldPaths(f.Type, prefix, out)
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			out[prefix+"."+name] = true
			fieldPaths(f.Type, prefix+"."+name, out)
		}
	default:
		// Scalar kinds have no JSON fields: nothing to list.
	}
}

// presentPaths lists the field paths a JSON document sets, guided by t.
func presentPaths(t reflect.Type, value any, prefix string, out map[string]bool) {
	switch t.Kind() { //nolint:exhaustive // default covers scalar kinds.
	case reflect.Pointer:
		presentPaths(t.Elem(), value, prefix, out)
	case reflect.Slice:
		items, _ := value.([]any)
		for _, item := range items {
			presentPaths(t.Elem(), item, prefix+"[]", out)
		}
	case reflect.Map:
		object, _ := value.(map[string]any)
		for _, item := range object {
			presentPaths(t.Elem(), item, prefix+".*", out)
		}
	case reflect.Struct:
		object, _ := value.(map[string]any)
		for f := range t.Fields() {
			if f.Anonymous {
				presentPaths(f.Type, value, prefix, out)
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if v, ok := object[name]; ok {
				out[prefix+"."+name] = true
				presentPaths(f.Type, v, prefix+"."+name, out)
			}
		}
	default:
		// Scalar kinds have no field paths: nothing to list.
	}
}

// An edge image must accept every configuration a released chart renders. The
// released-chart check renders the chart's ci values, so the full-config ci
// values must set every field the configuration has.
func TestFullChartValuesSetEveryConfigField(t *testing.T) {
	data, err := os.ReadFile("../../charts/argus/ci/full-config-values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The file is JSON after its comment header.
	data = data[bytes.IndexByte(data, '{'):]
	var values struct {
		Config map[string]any            `json:"config"`
		Checks map[string]map[string]any `json:"checks"`
	}
	if err := json.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	// Mirror the chart's argus.config template.
	rendered := values.Config
	rendered["listenAddress"], rendered["tokenFile"] = ":8443", "/var/run/argus-auth/token"
	rendered["tls"] = map[string]any{
		"certFile": "/var/run/argus-tls/tls.crt",
		"keyFile":  "/var/run/argus-tls/tls.key",
	}
	rendered["state"] = map[string]any{"namespace": "argus", "name": "argus-state"}
	checks := map[string]any{}
	for name, section := range values.Checks {
		if section["enabled"] != true {
			t.Fatalf("check %s is not enabled in the full-config values", name)
		}
		delete(section, "enabled")
		if name == "hosts" {
			section["privateKeyFile"], section["knownHostsFile"] = "/var/run/argus-ssh/ssh_private_key", "/config/known_hosts"
		}
		if name == "pbs" {
			section["tokenFile"], section["caFile"] = "/var/run/argus-pbs/token", "/config/pbs-ca.pem"
		}
		checks[name] = section
	}
	rendered["checks"] = checks
	encoded, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(encoded); err != nil {
		t.Fatal("full-config values do not load:", err)
	}
	all, present := map[string]bool{}, map[string]bool{}
	fieldPaths(reflect.TypeFor[Config](), "", all)
	presentPaths(reflect.TypeFor[Config](), rendered, "", present)
	var missing []string
	for path := range all {
		if !present[path] {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf(
			"charts/argus/ci/full-config-values.yaml does not set: %s",
			strings.Join(missing, ", "),
		)
	}
}

// chartManaged are configuration fields the chart sets itself; they are not
// chart values.
var chartManaged = map[string]bool{
	".checks.hosts.privateKeyFile": true, ".checks.hosts.knownHostsFile": true,
	".checks.pbs.tokenFile": true, ".checks.pbs.caFile": true,
}

// matchSchema requires every struct in t to be a closed schema object whose
// properties are exactly the struct's JSON fields, so a typo in a check
// section fails helm rendering instead of crash-looping the pod.
func matchSchema(
	t *testing.T,
	typ reflect.Type,
	schema map[string]any,
	path string,
	extra map[string]bool,
) {
	t.Helper()
	switch typ.Kind() { //nolint:exhaustive // default covers scalar kinds.
	case reflect.Pointer:
		matchSchema(t, typ.Elem(), schema, path, extra)
	case reflect.Slice:
		items, ok := schema["items"].(map[string]any)
		if !ok {
			t.Fatalf("%s: schema has no items", path)
		}
		matchSchema(t, typ.Elem(), items, path+"[]", nil)
	case reflect.Map:
		patterns, _ := schema["patternProperties"].(map[string]any)
		if len(patterns) != 1 || schema["additionalProperties"] != false {
			t.Fatalf("%s: map must be closed with one patternProperties entry", path)
		}
		for _, value := range patterns {
			pattern, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("%s: pattern is not an object", path)
			}
			matchSchema(t, typ.Elem(), pattern, path+".*", nil)
		}
	case reflect.Struct:
		matchObject(t, typ, schema, path, extra)
	default:
		// Scalars have no schema shape to match.
	}
}

// matchObject matches the schema against one configuration object.
func matchObject(
	t *testing.T,
	typ reflect.Type,
	schema map[string]any,
	path string,
	extra map[string]bool,
) {
	t.Helper()
	if schema["additionalProperties"] != false {
		t.Fatalf("%s: schema object is not closed", path)
	}
	properties, _ := schema["properties"].(map[string]any)
	want := map[string]reflect.Type{}
	collectFields(typ, path, want)
	matchPropertyShapes(t, properties, want, path, extra)
	matchPropertySchemas(t, properties, want, path)
}

// collectFields records each JSON field (recursively through embedded types).
func collectFields(typ reflect.Type, path string, want map[string]reflect.Type) {
	for f := range typ.Fields() {
		if f.Anonymous {
			collectFields(f.Type, path, want)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !chartManaged[path+"."+name] {
			want[name] = f.Type
		}
	}
}

func matchPropertyShapes(
	t *testing.T,
	properties map[string]any,
	want map[string]reflect.Type,
	path string,
	extra map[string]bool,
) {
	t.Helper()
	for name := range properties {
		if _, ok := want[name]; !ok && !extra[name] {
			t.Errorf("%s.%s: schema property has no configuration field", path, name)
		}
	}
}

func matchPropertySchemas(
	t *testing.T,
	properties map[string]any,
	want map[string]reflect.Type,
	path string,
) {
	t.Helper()
	for name, field := range want {
		child, ok := properties[name].(map[string]any)
		if !ok {
			t.Errorf("%s.%s: configuration field missing from the chart schema", path, name)
			continue
		}
		matchSchema(t, field, child, path+"."+name, nil)
	}
}

func TestChartSchemaClosesEveryCheckSection(t *testing.T) {
	data, err := os.ReadFile("../../charts/argus/values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema properties is not an object")
	}
	checks, ok := properties["checks"].(map[string]any)
	if !ok {
		t.Fatal("checks is not an object")
	}
	if checks["additionalProperties"] != false {
		t.Fatal("checks is not closed")
	}
	sections, ok := checks["properties"].(map[string]any)
	if !ok {
		t.Fatal("checks properties is not an object")
	}
	typ := reflect.TypeFor[Checks]()
	for f := range typ.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		section, ok := sections[name].(map[string]any)
		if !ok {
			t.Errorf("check %s missing from the chart schema", name)
			continue
		}
		matchSchema(t, f.Type, section, ".checks."+name, map[string]bool{"enabled": true})
	}
	if len(sections) != typ.NumField() {
		t.Errorf(
			"chart schema has %d check sections, configuration has %d",
			len(sections),
			typ.NumField(),
		)
	}
}
