package broker

import (
	"reflect"
	"testing"
)

func TestParseMCPServerNames(t *testing.T) {
	// Shape mirrors `codex mcp list --json`: an array of server objects. Only the
	// name is read; extra fields are ignored, and unsafe names are dropped.
	in := []byte(`[
      {"name":"llm-graphrag-context","enabled":true,"transport":{"type":"streamable_http"}},
      {"name":"playwright","enabled":true},
      {"name":"semanta","enabled":true},
      {"name":"unreal-ai","enabled":true},
      {"name":"weird name","enabled":true},
      {"name":"dotted.name","enabled":true},
      {"name":""}
    ]`)
	got := parseMCPServerNames(in)
	want := []string{"llm-graphrag-context", "playwright", "semanta", "unreal-ai"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseMCPServerNames = %v, want %v (unsafe/empty names must be dropped)", got, want)
	}
}

func TestParseMCPServerNamesBadJSON(t *testing.T) {
	for _, in := range [][]byte{[]byte(""), []byte("not json"), []byte("{}"), nil} {
		if got := parseMCPServerNames(in); got != nil {
			t.Fatalf("parseMCPServerNames(%q) = %v, want nil", in, got)
		}
	}
}

func TestMCPDisableArgs(t *testing.T) {
	got := mcpDisableArgs([]string{"llm-graphrag-context", "playwright"})
	want := []string{
		"-c", "mcp_servers.llm-graphrag-context.enabled=false",
		"-c", "mcp_servers.playwright.enabled=false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mcpDisableArgs = %v, want %v", got, want)
	}
	if got := mcpDisableArgs(nil); len(got) != 0 {
		t.Fatalf("mcpDisableArgs(nil) = %v, want empty", got)
	}
}

func TestIsSafeMCPName(t *testing.T) {
	cases := map[string]bool{
		"playwright":           true,
		"llm-graphrag-context": true,
		"unreal_ai":            true,
		"srv1":                 true,
		"":                     false,
		"weird name":           false,
		"dotted.name":          false,
		"has=eq":               false,
		`quoted"x`:             false,
	}
	for name, want := range cases {
		if got := isSafeMCPName(name); got != want {
			t.Errorf("isSafeMCPName(%q) = %v, want %v", name, got, want)
		}
	}
}
