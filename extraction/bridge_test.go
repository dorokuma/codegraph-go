package extraction

import "testing"

func TestDetectCGo(t *testing.T) {
	src := `
/*
#include <stdio.h>
void c_hello();
*/
import "C"

//export GoHello
func GoHello() {}

func main() {
	C.c_hello()
}
`
	d := NewCrossLanguageDetector()
	edges := d.Detect(src, "/main.go", "go")
	if len(edges) < 2 {
		t.Fatalf("expected cgo bridges, got %d: %+v", len(edges), edges)
	}
}

func TestDetectPythonCExt(t *testing.T) {
	src := `
import ctypes
lib = ctypes.CDLL("libfoo.so")
lib.do_work()
ffi.cdef("int add(int, int);")
cdef extern from "header.h"
`
	d := NewCrossLanguageDetector()
	edges := d.Detect(src, "/ext.py", "python")
	if len(edges) < 2 {
		t.Fatalf("expected python c ext bridges, got %d: %+v", len(edges), edges)
	}
}

func TestDetectNativeModules(t *testing.T) {
	src := `
NativeModules.Camera.takePhoto()
const { open } = NativeModules.FilePicker
requireNativeModule('ExpoHaptics')
import Spec from './NativeCamera'
`
	d := NewCrossLanguageDetector()
	edges := d.Detect(src, "/bridge.ts", "typescript")
	if len(edges) < 3 {
		t.Fatalf("expected rn bridges, got %d: %+v", len(edges), edges)
	}
}

func TestDetectSwiftObjCBridge(t *testing.T) {
	src := `
@objc
func sharedInstance() {}

@objcMembers
class Foo {
  func bar() {}
}
`
	d := NewCrossLanguageDetector()
	edges := d.Detect(src, "/Foo.swift", "swift")
	if len(edges) < 1 {
		t.Fatalf("expected swift->objc bridges, got %d", len(edges))
	}
}

func TestDetectObjCSwiftBridge(t *testing.T) {
	src := `
[manager startWithOptions:opts]
`
	d := NewCrossLanguageDetector()
	edges := d.Detect(src, "/App.m", "objective-c")
	if len(edges) < 1 {
		t.Fatalf("expected objc->swift bridges, got %d", len(edges))
	}
}

func TestDetectBridgeUnsupported(t *testing.T) {
	d := NewCrossLanguageDetector()
	if edges := d.Detect("x", "/a.rs", "rust"); edges != nil {
		t.Fatalf("expected nil, got %+v", edges)
	}
}
