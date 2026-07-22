package extraction

import (
	"strings"
	"testing"
)

func findNode(nodes []ExtractedNode, name string) *ExtractedNode {
	for i := range nodes {
		if nodes[i].Name == name {
			return &nodes[i]
		}
	}
	return nil
}

func TestGoMetadataFields(t *testing.T) {
	src := `package mypkg

func Hello(name string) error {
	return nil
}

func hidden() {}

type MyStruct struct {
	Name string
}

type myIface interface {
	Do() error
}

func (s *MyStruct) Method(x int) string {
	return Hello("x")
}

func (s MyStruct) other() {}
`
	ext := NewTreeSitterExtractor("go")
	if ext == nil {
		t.Fatal("tree-sitter go extractor unavailable")
	}
	res := ext.Extract(src, "foo.go")

	hello := findNode(res.Nodes, "Hello")
	if hello == nil {
		t.Fatal("missing Hello")
	}
	if hello.QualifiedName != "mypkg.Hello" {
		t.Errorf("Hello.QualifiedName = %q, want mypkg.Hello", hello.QualifiedName)
	}
	if !strings.Contains(hello.Signature, "(name string)") {
		t.Errorf("Hello.Signature = %q, want params", hello.Signature)
	}
	if !strings.Contains(hello.Signature, "error") {
		t.Errorf("Hello.Signature = %q, want error return", hello.Signature)
	}
	if !hello.IsExported {
		t.Error("Hello should be exported")
	}
	if hello.Visibility != "public" {
		t.Errorf("Hello.Visibility = %q", hello.Visibility)
	}
	if hello.ReturnType != "error" {
		t.Errorf("Hello.ReturnType = %q, want error", hello.ReturnType)
	}

	hidden := findNode(res.Nodes, "hidden")
	if hidden == nil {
		t.Fatal("missing hidden")
	}
	if hidden.IsExported {
		t.Error("hidden must not be exported")
	}
	if hidden.Visibility != "private" {
		t.Errorf("hidden.Visibility = %q", hidden.Visibility)
	}
	if hidden.QualifiedName != "mypkg.hidden" {
		t.Errorf("hidden.QualifiedName = %q", hidden.QualifiedName)
	}

	st := findNode(res.Nodes, "MyStruct")
	if st == nil {
		t.Fatal("missing MyStruct (type_spec extraction)")
	}
	if st.Kind != "struct" {
		t.Errorf("MyStruct.Kind = %q", st.Kind)
	}
	if st.QualifiedName != "mypkg.MyStruct" || !st.IsExported {
		t.Errorf("MyStruct meta: qn=%q exp=%v", st.QualifiedName, st.IsExported)
	}

	iface := findNode(res.Nodes, "myIface")
	if iface == nil {
		t.Fatal("missing myIface")
	}
	if iface.Kind != "interface" || iface.IsExported {
		t.Errorf("myIface kind=%q exp=%v", iface.Kind, iface.IsExported)
	}

	method := findNode(res.Nodes, "Method")
	if method == nil {
		t.Fatal("missing Method")
	}
	if method.Kind != "method" {
		t.Errorf("Method.Kind = %q", method.Kind)
	}
	if method.QualifiedName != "MyStruct.Method" {
		t.Errorf("Method.QualifiedName = %q, want MyStruct.Method", method.QualifiedName)
	}
	if !method.IsExported || method.ReturnType != "string" {
		t.Errorf("Method exp=%v ret=%q", method.IsExported, method.ReturnType)
	}
	if !strings.Contains(method.Signature, "(x int)") {
		t.Errorf("Method.Signature = %q", method.Signature)
	}

	// struct → method contains edge
	hasContains := false
	for _, e := range res.Edges {
		if e.Kind == "contains" && e.SourceName == "MyStruct" && e.TargetName == "Method" {
			hasContains = true
		}
	}
	if !hasContains {
		t.Error("missing contains edge MyStruct → Method")
	}
}

func TestTSJSMetadataFields(t *testing.T) {
	src := `
export function foo(a: number): string { return ""; }
function hidden() {}
export const Bar = (x: number) => x;
export class MyClass {
  public method(x: number): void {}
  private secret() {}
}
class Internal {}
`
	ext := NewTreeSitterExtractor("typescript")
	if ext == nil {
		t.Fatal("tree-sitter ts extractor unavailable")
	}
	res := ext.Extract(src, "app.ts")

	foo := findNode(res.Nodes, "foo")
	if foo == nil {
		t.Fatal("missing foo")
	}
	if !foo.IsExported {
		t.Error("foo should be exported")
	}
	if !strings.Contains(foo.Signature, "(a: number)") {
		t.Errorf("foo.Signature = %q", foo.Signature)
	}
	if foo.ReturnType != "string" {
		t.Errorf("foo.ReturnType = %q", foo.ReturnType)
	}
	if foo.QualifiedName != "foo" {
		t.Errorf("foo.QualifiedName = %q", foo.QualifiedName)
	}

	hidden := findNode(res.Nodes, "hidden")
	if hidden == nil || hidden.IsExported {
		t.Fatalf("hidden meta: %+v", hidden)
	}

	bar := findNode(res.Nodes, "Bar")
	if bar == nil {
		t.Fatal("missing Bar")
	}
	if !bar.IsExported {
		t.Error("Bar should be exported (export const)")
	}
	if bar.Kind != "component" && bar.Kind != "function" {
		t.Errorf("Bar.Kind = %q", bar.Kind)
	}

	cls := findNode(res.Nodes, "MyClass")
	if cls == nil || !cls.IsExported {
		t.Fatalf("MyClass meta: %+v", cls)
	}

	method := findNode(res.Nodes, "method")
	if method == nil {
		t.Fatal("missing method")
	}
	if method.QualifiedName != "MyClass.method" {
		t.Errorf("method.QualifiedName = %q", method.QualifiedName)
	}
	if method.Visibility != "public" {
		t.Errorf("method.Visibility = %q", method.Visibility)
	}
	if !method.IsExported {
		t.Error("public method on exported class should be exported")
	}
	if method.ReturnType != "void" {
		t.Errorf("method.ReturnType = %q", method.ReturnType)
	}

	secret := findNode(res.Nodes, "secret")
	if secret == nil {
		t.Fatal("missing secret")
	}
	if secret.Visibility != "private" || secret.IsExported {
		t.Errorf("secret vis=%q exp=%v", secret.Visibility, secret.IsExported)
	}

	internal := findNode(res.Nodes, "Internal")
	if internal == nil || internal.IsExported {
		t.Fatalf("Internal should exist and not be exported: %+v", internal)
	}
}

func TestRustMetadataFields(t *testing.T) {
	src := `
use crate::helper::run;

pub struct Widget {
    id: u32,
}

struct Hidden {}

impl Widget {
    pub fn new(id: u32) -> Widget {
        run();
        Widget { id }
    }

    fn paint(&self) {
        run();
    }
}

pub fn boot() {
    let w = Widget::new(1);
    w.paint();
}

fn internal() {}
`
	ext := NewExtractor("rust")
	res := ext.Extract(src, "lib.rs")

	w := findNode(res.Nodes, "Widget")
	if w == nil || w.Kind != "struct" || !w.IsExported {
		t.Fatalf("Widget: %+v", w)
	}

	hidden := findNode(res.Nodes, "Hidden")
	if hidden == nil || hidden.IsExported {
		t.Fatalf("Hidden: %+v", hidden)
	}

	newFn := findNode(res.Nodes, "new")
	if newFn == nil {
		t.Fatal("missing new")
	}
	if newFn.Kind != "method" {
		t.Errorf("new.Kind = %q, want method", newFn.Kind)
	}
	if newFn.QualifiedName != "Widget.new" {
		t.Errorf("new.QualifiedName = %q", newFn.QualifiedName)
	}
	if !newFn.IsExported {
		t.Error("pub fn new should be exported")
	}
	if !strings.Contains(newFn.Signature, "(id: u32)") || !strings.Contains(newFn.Signature, "-> Widget") {
		t.Errorf("new.Signature = %q", newFn.Signature)
	}
	if newFn.ReturnType != "Widget" {
		t.Errorf("new.ReturnType = %q", newFn.ReturnType)
	}

	paint := findNode(res.Nodes, "paint")
	if paint == nil || paint.IsExported || paint.Kind != "method" {
		t.Fatalf("paint: %+v", paint)
	}
	if paint.QualifiedName != "Widget.paint" {
		t.Errorf("paint.QualifiedName = %q", paint.QualifiedName)
	}

	boot := findNode(res.Nodes, "boot")
	if boot == nil || !boot.IsExported || boot.Kind != "function" {
		t.Fatalf("boot: %+v", boot)
	}
	if boot.QualifiedName != "boot" {
		t.Errorf("boot.QualifiedName = %q", boot.QualifiedName)
	}

	internal := findNode(res.Nodes, "internal")
	if internal == nil || internal.IsExported {
		t.Fatalf("internal: %+v", internal)
	}

	hasContains := false
	for _, e := range res.Edges {
		if e.Kind == "contains" && e.SourceName == "Widget" && e.TargetName == "new" {
			hasContains = true
		}
	}
	if !hasContains {
		t.Error("missing contains Widget → new")
	}
}

func TestPythonMetadataFields(t *testing.T) {
	src := `
def hello(name: str) -> int:
    return 1

def _private():
    pass

class MyClass:
    def method(self, x: int) -> str:
        return "a"

    def _hidden(self):
        pass
`
	ext := NewTreeSitterExtractor("python")
	if ext == nil {
		t.Fatal("tree-sitter python extractor unavailable")
	}
	res := ext.Extract(src, "main.py")

	hello := findNode(res.Nodes, "hello")
	if hello == nil {
		t.Fatal("missing hello")
	}
	if hello.QualifiedName != "hello" || !hello.IsExported {
		t.Errorf("hello qn=%q exp=%v", hello.QualifiedName, hello.IsExported)
	}
	if !strings.Contains(hello.Signature, "(name: str)") || !strings.Contains(hello.Signature, "-> int") {
		t.Errorf("hello.Signature = %q", hello.Signature)
	}
	if hello.ReturnType != "int" {
		t.Errorf("hello.ReturnType = %q", hello.ReturnType)
	}
	if hello.Visibility != "public" {
		t.Errorf("hello.Visibility = %q", hello.Visibility)
	}

	priv := findNode(res.Nodes, "_private")
	if priv == nil || priv.IsExported || priv.Visibility != "protected" {
		t.Fatalf("_private meta: %+v", priv)
	}

	cls := findNode(res.Nodes, "MyClass")
	if cls == nil || !cls.IsExported {
		t.Fatalf("MyClass: %+v", cls)
	}

	method := findNode(res.Nodes, "method")
	if method == nil {
		t.Fatal("missing method")
	}
	if method.Kind != "method" {
		t.Errorf("method.Kind = %q, want method", method.Kind)
	}
	if method.QualifiedName != "MyClass.method" {
		t.Errorf("method.QualifiedName = %q", method.QualifiedName)
	}
	if method.ReturnType != "str" {
		t.Errorf("method.ReturnType = %q", method.ReturnType)
	}

	hidden := findNode(res.Nodes, "_hidden")
	if hidden == nil || hidden.IsExported {
		t.Fatalf("_hidden: %+v", hidden)
	}

	hasContains := false
	for _, e := range res.Edges {
		if e.Kind == "contains" && e.SourceName == "MyClass" && e.TargetName == "method" {
			hasContains = true
		}
	}
	if !hasContains {
		t.Error("missing contains MyClass → method")
	}
}
