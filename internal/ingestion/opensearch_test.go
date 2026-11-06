package ingestion

import (
	"encoding/json"
	"strings"
	"testing"
)

var defaultFields = FieldMap{
	Level: "level", LevelValue: "ERROR", Message: "message",
	StackTrace: "stack_trace", Service: "service", Timestamp: "@timestamp",
}

var ecsFields = FieldMap{
	Level: "log.level", LevelValue: "error", Message: "message",
	StackTrace: "error.stack_trace", Service: "service.name", Timestamp: "@timestamp",
}

func TestMapDocumentFlat(t *testing.T) {
	src := `{"level":"ERROR","message":"boom","stack_trace":"at a\nat b","service":"api","@timestamp":"2026-01-01T00:00:00Z"}`
	in, ok, err := MapDocument(defaultFields, []byte(src))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if in.Message != "boom" || in.Service != "api" || in.Severity != "ERROR" || in.StackTrace != "at a\nat b" {
		t.Fatalf("unexpected mapping: %+v", in)
	}
}

func TestMapDocumentECSNested(t *testing.T) {
	src := `{"log":{"level":"error"},"message":"nil deref","error":{"stack_trace":"goroutine 1"},"service":{"name":"checkout"}}`
	in, ok, err := MapDocument(ecsFields, []byte(src))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if in.Service != "checkout" || in.StackTrace != "goroutine 1" || in.Severity != "error" {
		t.Fatalf("unexpected mapping: %+v", in)
	}
}

func TestMapDocumentECSFlatDottedKeys(t *testing.T) {
	src := `{"log.level":"error","message":"x","error.stack_trace":"trace","service.name":"svc"}`
	in, ok, _ := MapDocument(ecsFields, []byte(src))
	if !ok || in.Service != "svc" || in.StackTrace != "trace" {
		t.Fatalf("unexpected mapping: ok=%v %+v", ok, in)
	}
}

func TestMapDocumentStackTraceArrayAndMissingMessage(t *testing.T) {
	src := `{"stack_trace":["java.lang.NullPointerException","\tat Foo.bar(Foo.java:10)"]}`
	in, ok, _ := MapDocument(defaultFields, []byte(src))
	if !ok {
		t.Fatal("expected ok")
	}
	if !strings.Contains(in.StackTrace, "Foo.java:10") {
		t.Fatalf("array stack trace not joined: %q", in.StackTrace)
	}
	if in.Message != "java.lang.NullPointerException" {
		t.Fatalf("expected message fallback to first stack line, got %q", in.Message)
	}
}

func TestMapDocumentWithoutStackTraceIsSkipped(t *testing.T) {
	_, ok, err := MapDocument(defaultFields, []byte(`{"message":"no trace"}`))
	if err != nil || ok {
		t.Fatalf("expected skip, got ok=%v err=%v", ok, err)
	}
}

func TestBuildQueryUsesConfiguredFields(t *testing.T) {
	body, err := BuildQuery(ecsFields, 120)
	if err != nil {
		t.Fatal(err)
	}
	var q map[string]any
	if err := json.Unmarshal(body, &q); err != nil {
		t.Fatalf("query is not valid JSON: %v", err)
	}
	s := string(body)
	for _, want := range []string{`"log.level":"error"`, `"field":"error.stack_trace"`, `"gte":"now-120s"`} {
		if !strings.Contains(s, want) {
			t.Errorf("query missing %s: %s", want, s)
		}
	}
}
