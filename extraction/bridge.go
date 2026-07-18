package extraction

import (
	"regexp"
	"strings"
)

// BridgeEdge represents a cross-language call.
type BridgeEdge struct {
	SourceLang string // e.g., "go"
	TargetLang string // e.g., "c"
	SourceFunc string // Go function name
	TargetFunc string // C function name
	File       string
	Line       int
}

// CrossLanguageDetector detects cross-language calls.
type CrossLanguageDetector struct{}

// NewCrossLanguageDetector creates a new cross-language detector.
func NewCrossLanguageDetector() *CrossLanguageDetector {
	return &CrossLanguageDetector{}
}

// Detect detects cross-language calls in source code.
func (d *CrossLanguageDetector) Detect(source string, filePath string, language string) []BridgeEdge {
	switch language {
	case "go":
		return d.detectCGo(source, filePath)
	case "python":
		return d.detectPythonCExt(source, filePath)
	case "typescript", "javascript":
		return d.detectNativeModules(source, filePath)
	case "swift":
		return d.detectSwiftObjCBridge(source, filePath)
	case "objective-c":
		return d.detectObjCSwiftBridge(source, filePath)
	}
	return nil
}

// ---------- CGo detection ----------

var (
	// import "C"
	cgoImportRe = regexp.MustCompile(`import\s+"C"`)
	// C.functionName() or C.functionName
	cgoCallRe = regexp.MustCompile(`\bC\.(\w+)`)
	// //export GoFunctionName
	cgoExportRe = regexp.MustCompile(`//export\s+(\w+)`)
	// C function declarations in preamble: void func(), int func(), etc.
	cgoPreambleRe = regexp.MustCompile(`(?:void|int|char\*?|float|double|long|short|unsigned|struct\s+\w+)\s+(\w+)\s*\(`)
	// Go function declarations
	goFuncDeclRe = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s+)?(\w+)\s*\(`)
)

func (d *CrossLanguageDetector) detectCGo(source string, filePath string) []BridgeEdge {
	lines := strings.Split(source, "\n")
	var edges []BridgeEdge

	// First pass: find preamble (comment block before import "C")
	inPreamble := false
	preambleFuncs := make(map[string]bool)
	importCLine := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect preamble comment block
		if strings.HasPrefix(trimmed, "/*") {
			inPreamble = true
		}
		if inPreamble {
			if matches := cgoPreambleRe.FindStringSubmatch(trimmed); len(matches) > 1 {
				preambleFuncs[matches[1]] = true
			}
			if strings.Contains(trimmed, "*/") {
				inPreamble = false
			}
			continue
		}

		// Single-line comments before import "C" might also be preamble
		if strings.HasPrefix(trimmed, "//") && importCLine == -1 {
			if matches := cgoPreambleRe.FindStringSubmatch(trimmed); len(matches) > 1 {
				preambleFuncs[matches[1]] = true
			}
			continue
		}

		// Find import "C"
		if cgoImportRe.MatchString(trimmed) {
			importCLine = i
			break
		}
	}

	// Second pass: find C.func() calls and //export after import "C"
	currentFunc := ""
	for i := importCLine + 1; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// Track current Go function
		if matches := goFuncDeclRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			currentFunc = matches[1]
		}

		// Check for //export (Go function exported to C)
		if matches := cgoExportRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			edges = append(edges, BridgeEdge{
				SourceLang: "go",
				TargetLang: "c",
				SourceFunc: matches[1],
				TargetFunc: matches[1],
				File:       filePath,
				Line:       lineNum,
			})
			continue
		}

		// Check for C.functionName() calls
		if callMatches := cgoCallRe.FindAllStringSubmatch(trimmed, -1); len(callMatches) > 0 {
			for _, m := range callMatches {
				if len(m) > 1 {
					edges = append(edges, BridgeEdge{
						SourceLang: "go",
						TargetLang: "c",
						SourceFunc: currentFunc,
						TargetFunc: m[1],
						File:       filePath,
						Line:       lineNum,
					})
				}
			}
		}
	}

	return edges
}

// ---------- Python C extension detection ----------

var (
	// ctypes.cdll.LoadLibrary("lib.so") or ctypes.CDLL("lib.so")
	pythonCtypesLoadRe = regexp.MustCompile(`(\w+)\s*=\s*ctypes\.(?:cdll\.LoadLibrary|CDLL|WinDLL)\s*\(`)
	// variable.functionName( - matches any variable that was assigned a ctypes library
	pythonCtypesCallRe = regexp.MustCompile(`(\w+)\.(\w+)\s*\(`)
	// cffi: ffi.cdef("...") and ffi.dlopen("lib.so")
	pythonCffiRe = regexp.MustCompile(`ffi\.(?:cdef|dlopen)\s*\(`)
	// Cython: cdef extern from "header.h"
	pythonCythonRe = regexp.MustCompile(`cdef\s+extern\s+from\s+['"]([^'"]+)['"]`)
)

func (d *CrossLanguageDetector) detectPythonCExt(source string, filePath string) []BridgeEdge {
	lines := strings.Split(source, "\n")
	var edges []BridgeEdge

	// Track ctypes library variables
	libVars := make(map[string]bool)

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// Detect ctypes library load and track variable name
		if matches := pythonCtypesLoadRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			libVars[matches[1]] = true
			continue
		}

		// Detect calls on tracked library variables
		if callMatches := pythonCtypesCallRe.FindAllStringSubmatch(trimmed, -1); len(callMatches) > 0 {
			for _, m := range callMatches {
				if len(m) > 2 && libVars[m[1]] {
					edges = append(edges, BridgeEdge{
						SourceLang: "python",
						TargetLang: "c",
						SourceFunc: "",
						TargetFunc: m[2],
						File:       filePath,
						Line:       lineNum,
					})
				}
			}
		}

		// cffi detection
		if pythonCffiRe.MatchString(trimmed) {
			edges = append(edges, BridgeEdge{
				SourceLang: "python",
				TargetLang: "c",
				SourceFunc: "",
				TargetFunc: "cffi_bindgen",
				File:       filePath,
				Line:       lineNum,
			})
		}

		// Cython detection
		if matches := pythonCythonRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			edges = append(edges, BridgeEdge{
				SourceLang: "python",
				TargetLang: "c",
				SourceFunc: "",
				TargetFunc: "cython:" + matches[1],
				File:       filePath,
				Line:       lineNum,
			})
		}
	}

	return edges
}

// ---------- React Native / Native Module detection ----------

var (
	// NativeModules.ModuleName.method() or const { Module } = NativeModules
	rnNativeModulesRe = regexp.MustCompile(`NativeModules\.(\w+)\.(\w+)\s*\(`)
	// Destructuring: const { method } = NativeModules.Module
	rnNativeModulesDestructRe = regexp.MustCompile(`(?:const|let|var)\s*\{([^}]+)\}\s*=\s*NativeModules\.(\w+)`)
	// requireNativeModule('ModuleName')
	rnExpoModulesRe = regexp.MustCompile(`requireNativeModule\s*\(\s*['"]([^'"]+)['"]\s*\)`)
	// import M from './NativeM'
	rnTurboModulesRe = regexp.MustCompile(`import\s+\w+\s+from\s+['"]\.\/Native(\w+)['"]`)
)

func (d *CrossLanguageDetector) detectNativeModules(source string, filePath string) []BridgeEdge {
	lines := strings.Split(source, "\n")
	var edges []BridgeEdge

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// React Native legacy bridge - direct method call
		if matches := rnNativeModulesRe.FindStringSubmatch(trimmed); len(matches) > 2 {
			edges = append(edges, BridgeEdge{
				SourceLang: "javascript",
				TargetLang: "native",
				SourceFunc: "",
				TargetFunc: matches[1] + "." + matches[2],
				File:       filePath,
				Line:       lineNum,
			})
		}

		// React Native legacy bridge - destructured methods
		if matches := rnNativeModulesDestructRe.FindStringSubmatch(trimmed); len(matches) > 2 {
			moduleName := matches[2]
			methods := strings.Split(matches[1], ",")
			for _, m := range methods {
				m = strings.TrimSpace(m)
				if m != "" {
					edges = append(edges, BridgeEdge{
						SourceLang: "javascript",
						TargetLang: "native",
						SourceFunc: "",
						TargetFunc: moduleName + "." + m,
						File:       filePath,
						Line:       lineNum,
					})
				}
			}
		}

		// Expo Modules
		if matches := rnExpoModulesRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			edges = append(edges, BridgeEdge{
				SourceLang: "javascript",
				TargetLang: "native",
				SourceFunc: "",
				TargetFunc: matches[1],
				File:       filePath,
				Line:       lineNum,
			})
		}

		// TurboModules
		if matches := rnTurboModulesRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			edges = append(edges, BridgeEdge{
				SourceLang: "javascript",
				TargetLang: "native",
				SourceFunc: "",
				TargetFunc: matches[1],
				File:       filePath,
				Line:       lineNum,
			})
		}
	}

	return edges
}

// ---------- Swift → ObjC bridging ----------

var (
	// @objc annotation
	swiftObjCAnnotationRe = regexp.MustCompile(`@objc`)
	// @objcMembers annotation
	swiftObjCMembersRe = regexp.MustCompile(`@objcMembers`)
)

func (d *CrossLanguageDetector) detectSwiftObjCBridge(source string, filePath string) []BridgeEdge {
	lines := strings.Split(source, "\n")
	var edges []BridgeEdge

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// Check for @objc annotation (indicates ObjC exposure)
		if swiftObjCAnnotationRe.MatchString(trimmed) || swiftObjCMembersRe.MatchString(trimmed) {
			// Find the function/method declaration
			for j := i + 1; j < len(lines) && j < i+5; j++ {
				funcLine := strings.TrimSpace(lines[j])
				if matches := regexp.MustCompile(`func\s+(\w+)\s*\(`).FindStringSubmatch(funcLine); len(matches) > 1 {
					edges = append(edges, BridgeEdge{
						SourceLang: "swift",
						TargetLang: "objc",
						SourceFunc: matches[1],
						TargetFunc: matches[1],
						File:       filePath,
						Line:       lineNum,
					})
					break
				}
			}
		}
	}

	return edges
}

// ---------- ObjC → Swift bridging ----------

var (
	// ObjC calls to Swift: [obj fooWithBar:]
	objcSwiftCallRe = regexp.MustCompile(`\[(\w+)\s+(\w+)(?::\s*\w+)?\]`)
)

func (d *CrossLanguageDetector) detectObjCSwiftBridge(source string, filePath string) []BridgeEdge {
	lines := strings.Split(source, "\n")
	var edges []BridgeEdge

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// ObjC message send
		if matches := objcSwiftCallRe.FindStringSubmatch(trimmed); len(matches) > 2 {
			edges = append(edges, BridgeEdge{
				SourceLang: "objc",
				TargetLang: "swift",
				SourceFunc: matches[1],
				TargetFunc: matches[2],
				File:       filePath,
				Line:       lineNum,
			})
		}
	}

	return edges
}
