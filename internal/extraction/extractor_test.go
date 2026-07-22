package extraction

import (
	"testing"
)

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"main.go", "go"},
		{"app.ts", "typescript"},
		{"app.tsx", "typescript"},
		{"index.js", "javascript"},
		{"index.jsx", "javascript"},
		{"index.mjs", "javascript"},
		{"main.py", "python"},
		{"lib.rs", "rust"},
		{"Main.java", "java"},
		{"Program.cs", "csharp"},
		{"app.rb", "ruby"},
		{"index.php", "php"},
		{"main.c", "c"},
		{"main.h", "c"},
		{"main.cpp", "cpp"},
		{"main.cc", "cpp"},
		{"main.cxx", "cpp"},
		{"main.hpp", "cpp"},
		{"app.swift", "swift"},
		{"app.kt", "kotlin"},
		{"app.kts", "kotlin"},
		{"Main.scala", "scala"},
		{"main.dart", "dart"},
		{"main.lua", "lua"},
		{"script.r", "r"},
		{"script.R", "r"},
		{"App.m", "objective-c"},
		{"App.mm", "objective-c"},
		{"Widget.svelte", "svelte"},
		{"App.vue", "vue"},
		{"page.astro", "astro"},
		{"theme.liquid", "liquid"},
		{"module.luau", "luau"},
		{"Unit1.pas", "pascal"},
		{"README.md", ""},
		{"style.css", ""},
		{"data.json", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := DetectLanguage(tt.path)
			if got != tt.want {
				t.Errorf("DetectLanguage(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsSupportedLanguage(t *testing.T) {
	supported := []string{
		"go", "typescript", "javascript", "python", "rust", "java",
		"csharp", "ruby", "php", "c", "cpp", "swift", "kotlin",
		"scala", "dart", "lua", "luau", "r", "objective-c", "svelte",
		"vue", "astro", "liquid", "pascal",
	}
	for _, lang := range supported {
		if !IsSupportedLanguage(lang) {
			t.Errorf("expected %q to be supported", lang)
		}
	}

	unsupported := []string{"html", "css", "json", "yaml", "xml", ""}
	for _, lang := range unsupported {
		if IsSupportedLanguage(lang) {
			t.Errorf("expected %q to be unsupported", lang)
		}
	}
}

func TestExtractGo(t *testing.T) {
	source := `package main

import "fmt"

func hello() {
	fmt.Println("hello")
}

func world() {
	hello()
}

type MyStruct struct {
	Name string
}

func (s MyStruct) Method() {
	hello()
}
`
	ext := NewExtractor("go")
	res := ext.Extract(source, "/test.go")

	// Check nodes
	nodeNames := make(map[string]bool)
	for _, n := range res.Nodes {
		nodeNames[n.Name] = true
		if n.File != "/test.go" {
			t.Errorf("wrong file: %s", n.File)
		}
	}

	expectedNodes := []string{"hello", "world", "MyStruct", "Method"}
	for _, name := range expectedNodes {
		if !nodeNames[name] {
			t.Errorf("missing node: %s", name)
		}
	}

	// Calls are unresolved refs (step 2), not live edges.
	hasCall := false
	for _, r := range res.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "world" && r.ReferenceName == "hello" {
			hasCall = true
		}
	}
	if !hasCall {
		t.Error("missing ref: world -> hello")
	}

	// Check import edge
	hasImport := false
	for _, e := range res.Edges {
		if e.Kind == "imports" && e.TargetName == "fmt" {
			hasImport = true
		}
	}
	if !hasImport {
		t.Error("missing import: fmt")
	}
}

func TestExtractJS(t *testing.T) {
	source := `import React from 'react';

function App() {
	return <div>Hello</div>;
}

class MyComponent extends React.Component {
	render() {
		return App();
	}
}
`
	ext := NewExtractor("javascript")
	res := ext.Extract(source, "/app.js")

	nodeNames := make(map[string]bool)
	for _, n := range res.Nodes {
		nodeNames[n.Name] = true
	}

	if !nodeNames["App"] {
		t.Error("missing node: App")
	}
	if !nodeNames["MyComponent"] {
		t.Error("missing node: MyComponent")
	}

	hasImport := false
	for _, e := range res.Edges {
		if e.Kind == "imports" && e.TargetName == "react" {
			hasImport = true
		}
	}
	if !hasImport {
		t.Error("missing import: react")
	}
}

func TestExtractPython(t *testing.T) {
	source := `import os
from pathlib import Path

def hello():
    print("hello")

def world():
    hello()

class MyClass:
    def method(self):
        hello()
`
	ext := NewExtractor("python")
	res := ext.Extract(source, "/main.py")

	nodeNames := make(map[string]bool)
	for _, n := range res.Nodes {
		nodeNames[n.Name] = true
	}

	expectedNodes := []string{"hello", "world", "MyClass"}
	for _, name := range expectedNodes {
		if !nodeNames[name] {
			t.Errorf("missing node: %s", name)
		}
	}

	hasCall := false
	for _, r := range res.Refs {
		if r.ReferenceKind == "calls" && r.FromName == "world" && r.ReferenceName == "hello" {
			hasCall = true
		}
	}
	if !hasCall {
		t.Error("missing ref: world -> hello")
	}
}

func TestExtractGeneric(t *testing.T) {
	source := `fn main() {
    println!("hello");
}

struct MyStruct {
    name: String,
}
`
	ext := NewExtractor("rust")
	res := ext.Extract(source, "/main.rs")

	nodeNames := make(map[string]bool)
	for _, n := range res.Nodes {
		nodeNames[n.Name] = true
	}

	if !nodeNames["main"] {
		t.Error("missing node: main")
	}
	if !nodeNames["MyStruct"] {
		t.Error("missing node: MyStruct")
	}
}

func TestFindBraceEnd(t *testing.T) {
	tests := []struct {
		lines []string
		start int
		want  int
	}{
		{
			lines: []string{"func foo() {", "	return 1", "}"},
			start: 0,
			want:  3,
		},
		{
			lines: []string{"func bar() {", "	if true {", "		return 1", "	}", "}"},
			start: 0,
			want:  5,
		},
		{
			lines: []string{"func baz() {", "	return", "}"},
			start: 0,
			want:  3,
		},
	}

	for _, tt := range tests {
		got := findBraceEnd(tt.lines, tt.start)
		if got != tt.want {
			t.Errorf("findBraceEnd: got %d, want %d", got, tt.want)
		}
	}
}

func TestFindIndentEnd(t *testing.T) {
	tests := []struct {
		lines []string
		start int
		want  int
	}{
		{
			lines: []string{"def foo():", "    return 1"},
			start: 0,
			want:  2,
		},
		{
			lines: []string{"def bar():", "    if True:", "        return 1", "    return 2"},
			start: 0,
			want:  4,
		},
		{
			lines: []string{"def baz():", "    return 1", "def other():", "    pass"},
			start: 0,
			want:  2,
		},
	}

	for _, tt := range tests {
		got := findIndentEnd(tt.lines, tt.start)
		if got != tt.want {
			t.Errorf("findIndentEnd: got %d, want %d", got, tt.want)
		}
	}
}

func TestIsGoKeyword(t *testing.T) {
	keywords := []string{"if", "for", "return", "func", "var", "const", "type", "struct", "interface"}
	for _, kw := range keywords {
		if !isGoKeyword(kw) {
			t.Errorf("expected %q to be Go keyword", kw)
		}
	}
	if isGoKeyword("myFunc") {
		t.Error("myFunc should not be a keyword")
	}
}

func TestIsJSKeyword(t *testing.T) {
	keywords := []string{"if", "for", "return", "function", "class", "const", "let", "var"}
	for _, kw := range keywords {
		if !isJSKeyword(kw) {
			t.Errorf("expected %q to be JS keyword", kw)
		}
	}
	if isJSKeyword("myFunc") {
		t.Error("myFunc should not be a keyword")
	}
}

func TestIsPythonKeyword(t *testing.T) {
	keywords := []string{"if", "for", "return", "def", "class", "import", "from"}
	for _, kw := range keywords {
		if !isPythonKeyword(kw) {
			t.Errorf("expected %q to be Python keyword", kw)
		}
	}
	if isPythonKeyword("my_func") {
		t.Error("my_func should not be a keyword")
	}
}

func TestExtractEmptySource(t *testing.T) {
	ext := NewExtractor("go")
	res := ext.Extract("", "/empty.go")
	if len(res.Nodes) != 0 {
		t.Errorf("expected 0 nodes, got %d", len(res.Nodes))
	}
	if len(res.Edges) != 0 || len(res.Refs) != 0 {
		t.Errorf("expected 0 edges/refs, got edges=%d refs=%d", len(res.Edges), len(res.Refs))
	}
}

func TestExtractObjC(t *testing.T) {
	src := `
#import <Foundation/Foundation.h>
@interface MyClass : NSObject
@end
- (void)doWork;
`
	ext := NewExtractor("objective-c")
	res := ext.Extract(src, "/App.m")
	if len(res.Nodes) < 2 {
		t.Fatalf("expected objc nodes, got %d", len(res.Nodes))
	}
	if len(res.Edges) < 1 {
		t.Fatalf("expected import edge, got %d", len(res.Edges))
	}
}

func TestExtractFrontendComponent(t *testing.T) {
	src := `<script>
function hello() { return 1 }
</script>
<template><div></div></template>`
	ext := NewExtractor("vue")
	res := ext.Extract(src, "/App.vue")
	found := false
	for _, n := range res.Nodes {
		if n.Name == "hello" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected hello function in vue script, got %+v", res.Nodes)
	}
}

func TestExtractLuaPascalLiquid(t *testing.T) {
	lua := `
function greet() end
function Obj:method() end
require("mod")
`
	pas := `
unit MyUnit;
type TFoo = class
end;
function Bar: Integer;
procedure Baz;
`
	liq := `{% section 'header' %} {% snippet 'card' %}`
	if r := NewExtractor("luau").Extract(lua, "/a.luau"); len(r.Nodes) < 2 || len(r.Edges) < 1 {
		t.Fatalf("luau nodes=%d edges=%d", len(r.Nodes), len(r.Edges))
	}
	if r := NewExtractor("pascal").Extract(pas, "/a.pas"); len(r.Nodes) < 3 {
		t.Fatalf("pascal nodes=%d", len(r.Nodes))
	}
	if r := NewExtractor("liquid").Extract(liq, "/a.liquid"); len(r.Nodes) < 2 {
		t.Fatalf("liquid nodes=%d", len(r.Nodes))
	}
}
