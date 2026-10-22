package monitor

import (
	"embed"
	"encoding/json"
	"strings"
)

//go:embed protocol/*
var protocolFiles embed.FS

func SchemaJSON() []byte      { b, _ := protocolFiles.ReadFile("protocol/schema.json"); return b }
func ProtocolSamples() []byte { b, _ := protocolFiles.ReadFile("protocol/samples.json"); return b }

func IntegrationPrompt(executable, directory string) string {
	b, _ := protocolFiles.ReadFile("protocol/integration-prompt.md")
	var schema map[string]json.RawMessage
	_ = json.Unmarshal(SchemaJSON(), &schema)
	var defs map[string]json.RawMessage
	_ = json.Unmarshal(schema["$defs"], &defs)
	var input map[string]json.RawMessage
	_ = json.Unmarshal(defs["input"], &input)
	input["$schema"] = schema["$schema"]
	pretty, _ := json.MarshalIndent(input, "", "  ")
	return strings.NewReplacer("{{EXE}}", executable, "{{DATA_DIR}}", directory, "{{INPUT_SCHEMA}}", string(pretty)).Replace(string(b))
}
