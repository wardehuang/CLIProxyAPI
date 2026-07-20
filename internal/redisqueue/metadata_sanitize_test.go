package redisqueue

import "testing"

func TestCloneJSONSafeMetadataDropsFuncsAndKeepsScalars(t *testing.T) {
	source := map[string]any{
		"project": "cpa",
		"count":   2,
		"flag":    true,
		"cb":      func(string) {},
		"ch":      make(chan int),
	}
	cloned := cloneJSONSafeMetadata(source)
	if cloned == nil {
		t.Fatal("cloned metadata is nil")
	}
	if cloned["project"] != "cpa" {
		t.Fatalf("project = %v, want cpa", cloned["project"])
	}
	count, ok := cloned["count"].(float64)
	if !ok || count != 2 {
		t.Fatalf("count = %v (%T), want 2 float64", cloned["count"], cloned["count"])
	}
	if _, ok := cloned["cb"]; ok {
		t.Fatalf("func callback should be dropped")
	}
	if _, ok := cloned["ch"]; ok {
		t.Fatalf("chan should be dropped")
	}
	if _, ok := cloned["flag"].(bool); !ok {
		t.Fatalf("flag type = %T, want bool", cloned["flag"])
	}
}

func TestCloneJSONSafeMetadataReturnsNilForEmptyOrAllUnsafe(t *testing.T) {
	if cloneJSONSafeMetadata(nil) != nil {
		t.Fatal("nil source should return nil")
	}
	if cloneJSONSafeMetadata(map[string]any{"cb": func() {}}) != nil {
		t.Fatal("all-unsafe source should return nil")
	}
}
