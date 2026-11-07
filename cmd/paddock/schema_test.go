package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/sdougbrown/cli-contract"
)

// TestSchemaCommandEmitsAConformingDocument pins the one acceptance criterion a
// consumer depends on: `paddock schema` writes one plain document, and that
// document satisfies the published v1 schema. A command that grows a member the
// spec does not allow, or declares a flag the schema rejects, fails here rather
// than in a consumer's parser.
func TestSchemaCommandEmitsAConformingDocument(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"schema"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	var doc any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not one JSON value: %v\n%s", err, stdout.String())
	}
	if err := specSchema(t).Validate(doc); err != nil {
		t.Fatalf("the emitted document does not satisfy the v1 spec schema:\n%v", err)
	}
}

// specSchema compiles the published v1 document schema from the cli-contract
// source the build imports, so the test reads the same file the library ships
// rather than a copy of it that could drift.
func specSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	schema, err := jsonschema.NewCompiler().Compile(filepath.Join(cliContractDir(t), "spec", "cli-contract.schema.json"))
	if err != nil {
		t.Fatalf("compile the cli-contract spec schema: %v", err)
	}
	return schema
}

// cliContractDir resolves the cli-contract module directory from the compiled
// library: the module cache is where its source, spec schema included, sits.
func cliContractDir(t *testing.T) string {
	t.Helper()
	fn := runtime.FuncForPC(reflect.ValueOf(clicontract.Run).Pointer())
	if fn == nil {
		t.Fatal("cli-contract.Run carries no function metadata to locate the module")
	}
	file, _ := fn.FileLine(fn.Entry())
	return filepath.Dir(file)
}
