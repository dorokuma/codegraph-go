package extraction

import (
	"regexp"
	"strings"
)

// RouteNode represents a framework route.
type RouteNode struct {
	Method  string // GET, POST, PUT, DELETE, etc.
	Path    string // /api/users/:id
	Handler string // handler function name (empty for reflective frameworks)
	File    string
	Line    int
	// QualifiedName optional join key (e.g. GoFrame ::goframe-route:pkg.Type).
	QualifiedName string
}

// FrameworkDetector detects web framework routes in source code.
type FrameworkDetector struct{}

// NewFrameworkDetector creates a new framework detector.
func NewFrameworkDetector() *FrameworkDetector {
	return &FrameworkDetector{}
}

// DetectRoutes detects routes in source code for the given language.
func (d *FrameworkDetector) DetectRoutes(source string, filePath string, language string) []RouteNode {
	switch language {
	case "go":
		return d.detectGoRoutes(source, filePath)
	case "typescript", "javascript":
		return d.detectJSRoutes(source, filePath)
	case "python":
		return d.detectPythonRoutes(source, filePath)
	case "php":
		return d.detectPHPRoutes(source, filePath)
	case "ruby":
		return d.detectRubyRoutes(source, filePath)
	case "java":
		return d.detectJavaRoutes(source, filePath)
	case "csharp":
		return d.detectCSharpRoutes(source, filePath)
	case "rust":
		return d.detectRustRoutes(source, filePath)
	case "swift":
		return d.detectSwiftRoutes(source, filePath)
	case "scala":
		return d.detectScalaRoutes(source, filePath)
	}
	return nil
}

// ---------- Go frameworks: Gin, chi, gorilla/mux ----------

var (
	// Gin: r.GET("/path", handler)
	ginRouteRe = regexp.MustCompile(`(\w+)\.(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS|Any)\s*\(\s*"([^"]+)"\s*,\s*(\w+)`)
	// chi: r.Get("/path", handler), r.Post("/path", handler)
	chiRouteRe = regexp.MustCompile(`(\w+)\.(Get|Post|Put|Delete|Patch|Head|Options|Handle|HandleFunc)\s*\(\s*"([^"]+)"\s*,\s*(\w+)`)
	// mux: r.HandleFunc("/path", handler).Methods("GET", "POST")
	muxRouteRe = regexp.MustCompile(`(\w+)\.HandleFunc\s*\(\s*"([^"]+)"\s*,\s*(\w+)\s*\)\.Methods\s*\(\s*"([^"]+)"\s*(?:,\s*"[^"]+")*\s*\)`)
	// mux: r.Methods("GET").Path("/path").HandlerFunc(handler)
	muxRouteRe2 = regexp.MustCompile(`(\w+)\.Methods\s*\(\s*"(\w+)"\s*\)\.Path\s*\(\s*"([^"]+)"\s*\)\.HandlerFunc\s*\(\s*(\w+)`)
)

// GoFrame route marker embedded in route.qualified_name for synthesizer join.
const GoFrameRouteMarker = "::goframe-route:"

var (
	// type SignInReq struct { g.Meta `path:"/x" method:"post"` ...
	goframeMetaRe   = regexp.MustCompile("(?s)\\btype\\s+([A-Z]\\w*)\\s+struct\\s*\\{\\s*g\\.Meta\\s+`([^`]*)`")
	goframePathRe   = regexp.MustCompile(`\bpath:"([^"]+)"`)
	goframeMethodRe = regexp.MustCompile(`\bmethod:"([^"]+)"`)
	goPackageRe     = regexp.MustCompile(`(?m)^\s*package\s+(\w+)`)
)

func (d *FrameworkDetector) detectGoRoutes(source string, filePath string) []RouteNode {
	lines := strings.Split(source, "\n")
	var routes []RouteNode

	// GoFrame reflective routes (g.Meta tags) — no static handler name.
	routes = append(routes, d.detectGoFrameRoutes(source, filePath)...)

	for i, line := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		// Gin
		if matches := ginRouteRe.FindStringSubmatch(trimmed); len(matches) > 4 {
			routes = append(routes, RouteNode{
				Method:  matches[2],
				Path:    matches[3],
				Handler: matches[4],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}

		// chi
		if matches := chiRouteRe.FindStringSubmatch(trimmed); len(matches) > 4 {
			method := strings.ToUpper(matches[2])
			if method == "HANDLE" || method == "HANDLEFUNC" {
				// Handle/HandleFunc: try to find .Methods() on the same or next lines
				fullCall := trimmed
				for j := i + 1; j < len(lines) && j < i+3; j++ {
					fullCall += " " + strings.TrimSpace(lines[j])
				}
				methodsRe := regexp.MustCompile(`\.Methods\s*\(\s*"([^"]+)"`)
				if m := methodsRe.FindStringSubmatch(fullCall); len(m) > 1 {
					routes = append(routes, RouteNode{
						Method:  strings.ToUpper(m[1]),
						Path:    matches[3],
						Handler: matches[4],
						File:    filePath,
						Line:    lineNum,
					})
				}
				continue
			}
			routes = append(routes, RouteNode{
				Method:  method,
				Path:    matches[3],
				Handler: matches[4],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}

		// mux - pattern 1
		if matches := muxRouteRe.FindStringSubmatch(trimmed); len(matches) > 4 {
			routes = append(routes, RouteNode{
				Method:  matches[4],
				Path:    matches[2],
				Handler: matches[3],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}

		// mux - pattern 2
		if matches := muxRouteRe2.FindStringSubmatch(trimmed); len(matches) > 4 {
			routes = append(routes, RouteNode{
				Method:  matches[2],
				Path:    matches[3],
				Handler: matches[4],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}
	}

	return routes
}

// detectGoFrameRoutes finds request types with routable g.Meta tags.
// Handler is empty; synthesizer joins on request type in QualifiedName.
func (d *FrameworkDetector) detectGoFrameRoutes(source, filePath string) []RouteNode {
	if !strings.Contains(source, "g.Meta") {
		return nil
	}
	pkg := ""
	if m := goPackageRe.FindStringSubmatch(source); len(m) > 1 {
		pkg = m[1]
	}
	var routes []RouteNode
	for _, match := range goframeMetaRe.FindAllStringSubmatchIndex(source, -1) {
		if len(match) < 6 {
			continue
		}
		reqType := source[match[2]:match[3]]
		tag := source[match[4]:match[5]]
		pm := goframePathRe.FindStringSubmatch(tag)
		if len(pm) < 2 {
			continue // response mime-only g.Meta
		}
		method := "ANY"
		if mm := goframeMethodRe.FindStringSubmatch(tag); len(mm) > 1 {
			method = strings.ToUpper(mm[1])
		}
		line := 1 + strings.Count(source[:match[0]], "\n")
		joinKey := reqType
		if pkg != "" {
			joinKey = pkg + "." + reqType
		}
		routes = append(routes, RouteNode{
			Method:        method,
			Path:          pm[1],
			File:          filePath,
			Line:          line,
			QualifiedName: filePath + GoFrameRouteMarker + joinKey,
		})
	}
	return routes
}

// ---------- JS frameworks: Express, NestJS ----------

var (
	// Express: app.get("/path", handler), router.post("/path", handler)
	expressRouteRe = regexp.MustCompile(`(\w+)\.(get|post|put|delete|patch|head|options|all)\s*\(\s*['"]([^'"]+)['"]\s*,\s*(\w+)`)
	// Express with middleware: app.get("/path", middleware1, middleware2, ..., handler)
	expressRouteMiddlewareRe = regexp.MustCompile(`(\w+)\.(get|post|put|delete|patch|head|options|all)\s*\(\s*['"]([^'"]+)['"]\s*,\s*(?:\w+\s*,\s*)*(\w+)`)
	// NestJS: @Get("/path"), @Post("/path"), @Controller("prefix")
	nestjsRouteRe = regexp.MustCompile(`@(Get|Post|Put|Delete|Patch|Options|Head|All)\s*\(\s*['"]?([^'")\s]+)['"]?\s*\)`)
	// NestJS Controller prefix
	nestjsControllerRe = regexp.MustCompile(`@Controller\s*\(\s*['"]([^'"]+)['"]\s*\)`)
	// React Router v5/v6 JSX: <Route path="/x" component={Comp}/> or element={<Comp/>}
	reactRouteTagRe = regexp.MustCompile(`<Route\b`)
	reactRoutePathRe = regexp.MustCompile(`\bpath\s*=\s*["']([^"']+)["']`)
	reactRouteCompRe = regexp.MustCompile(`\bcomponent\s*=\s*\{\s*([A-Z][A-Za-z0-9_]*)`)
	reactRouteElemRe = regexp.MustCompile(`\belement\s*=\s*\{\s*<\s*([A-Z][A-Za-z0-9_]*)`)
	// React Router data API: path: '/x', element: <Comp/> / Component: Comp
	reactDataRouterAPIRe = regexp.MustCompile(`\b(?:createBrowserRouter|createHashRouter|createMemoryRouter|createRoutesFromElements)\b`)
	reactDataPathRe      = regexp.MustCompile(`\bpath\s*:\s*['"]([^'"]*)['"]`)
	reactDataElemRe      = regexp.MustCompile(`\belement\s*:\s*<\s*([A-Z][A-Za-z0-9_]*)`)
	reactDataCompRe      = regexp.MustCompile(`\bComponent\s*:\s*([A-Z][A-Za-z0-9_]*)`)
)

func (d *FrameworkDetector) detectJSRoutes(source string, filePath string) []RouteNode {
	lines := strings.Split(source, "\n")
	var routes []RouteNode
	controllerPrefix := ""

	for i, line := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		// NestJS Controller prefix
		if matches := nestjsControllerRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			controllerPrefix = matches[1]
			continue
		}

		// NestJS route decorators
		if matches := nestjsRouteRe.FindStringSubmatch(trimmed); len(matches) > 2 {
			handler := extractJSHandler(lines, i)
			if handler != "" {
				routes = append(routes, RouteNode{
					Method:  strings.ToUpper(matches[1]),
					Path:    controllerPrefix + matches[2],
					Handler: handler,
					File:    filePath,
					Line:    lineNum,
				})
			}
			continue
		}

		// Express with middleware (check first - more specific)
		if matches := expressRouteMiddlewareRe.FindStringSubmatch(trimmed); len(matches) > 4 {
			routes = append(routes, RouteNode{
				Method:  strings.ToUpper(matches[2]),
				Path:    matches[3],
				Handler: matches[4],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}

		// Express basic
		if matches := expressRouteRe.FindStringSubmatch(trimmed); len(matches) > 4 {
			routes = append(routes, RouteNode{
				Method:  strings.ToUpper(matches[2]),
				Path:    matches[3],
				Handler: matches[4],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}
	}

	// React Router JSX + data-router objects (whole-file scan; multi-line tags).
	routes = append(routes, detectReactRouterRoutes(source, filePath)...)

	return routes
}

// detectReactRouterRoutes extracts React Router route nodes (GET-style) linked to components.
func detectReactRouterRoutes(source, filePath string) []RouteNode {
	var routes []RouteNode
	// <Route …>
	for _, idx := range reactRouteTagRe.FindAllStringIndex(source, -1) {
		end := idx[0] + 400
		if end > len(source) {
			end = len(source)
		}
		window := source[idx[0]:end]
		pm := reactRoutePathRe.FindStringSubmatch(window)
		if pm == nil {
			continue
		}
		handler := ""
		if cm := reactRouteCompRe.FindStringSubmatch(window); cm != nil {
			handler = cm[1]
		} else if em := reactRouteElemRe.FindStringSubmatch(window); em != nil {
			handler = em[1]
		}
		line := strings.Count(source[:idx[0]], "\n") + 1
		routes = append(routes, RouteNode{
			Method:  "GET",
			Path:    pm[1],
			Handler: handler,
			File:    filePath,
			Line:    line,
		})
	}
	// createBrowserRouter([{ path, element }])
	if reactDataRouterAPIRe.MatchString(source) {
		for _, m := range reactDataPathRe.FindAllStringSubmatchIndex(source, -1) {
			if len(m) < 4 {
				continue
			}
			end := m[0] + 300
			if end > len(source) {
				end = len(source)
			}
			win := source[m[0]:end]
			handler := ""
			if em := reactDataElemRe.FindStringSubmatch(win); em != nil {
				handler = em[1]
			} else if cm := reactDataCompRe.FindStringSubmatch(win); cm != nil {
				handler = cm[1]
			}
			if handler == "" {
				continue // require component → real route object
			}
			path := source[m[2]:m[3]]
			if path == "" {
				path = "/"
			}
			line := strings.Count(source[:m[0]], "\n") + 1
			routes = append(routes, RouteNode{
				Method:  "GET",
				Path:    path,
				Handler: handler,
				File:    filePath,
				Line:    line,
			})
		}
	}
	return routes
}

// extractJSHandler extracts the function name from the line after a decorator.
func extractJSHandler(lines []string, decoratorLine int) string {
	// Look for the function/method declaration after the decorator
	for i := decoratorLine + 1; i < len(lines) && i < decoratorLine+5; i++ {
		line := strings.TrimSpace(lines[i])
		// async handler() or handler()
		if matches := regexp.MustCompile(`(?:async\s+)?(?:function\s+)?(\w+)\s*\(`).FindStringSubmatch(line); len(matches) > 1 {
			name := matches[1]
			if name != "class" && name != "function" {
				return name
			}
		}
	}
	return ""
}

// ---------- Python frameworks: Flask, FastAPI, Django ----------

var (
	// Flask: @app.route("/path", methods=["GET"])
	flaskRouteRe = regexp.MustCompile(`@(\w+)\.route\s*\(\s*['"]([^'"]+)['"]\s*(?:,\s*methods\s*=\s*\[([^\]]+)\])?\s*\)`)
	// FastAPI/Flask shortcut: @app.get("/path"), @router.post("/path")
	fastapiRouteRe = regexp.MustCompile(`@(\w+)\.(get|post|put|delete|patch|head|options)\s*\(\s*['"]([^'"]+)['"]\s*\)`)
	// Django: path("url", view), re_path("url", view)
	djangoRouteRe = regexp.MustCompile(`(?:path|re_path)\s*\(\s*['"]([^'"]+)['"]\s*,\s*([\w.]+)`)
)

func (d *FrameworkDetector) detectPythonRoutes(source string, filePath string) []RouteNode {
	lines := strings.Split(source, "\n")
	var routes []RouteNode

	for i, line := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		// Flask route decorator
		if matches := flaskRouteRe.FindStringSubmatch(trimmed); len(matches) > 2 {
			methods := "GET"
			if matches[3] != "" {
				methods = strings.ToUpper(matches[3])
				methods = strings.ReplaceAll(methods, "'", "")
				methods = strings.ReplaceAll(methods, "\"", "")
				methods = strings.TrimSpace(methods)
			}
			// The handler is the next line's function
			handler := extractPythonHandler(lines, i)
			if handler != "" {
				// Split multiple methods into separate routes
				for _, m := range strings.Split(methods, ",") {
					m = strings.TrimSpace(m)
					if m != "" {
						routes = append(routes, RouteNode{
							Method:  m,
							Path:    matches[2],
							Handler: handler,
							File:    filePath,
							Line:    lineNum,
						})
					}
				}
			}
			continue
		}

		// FastAPI/Flask shortcut (combined - same regex pattern)
		if matches := fastapiRouteRe.FindStringSubmatch(trimmed); len(matches) > 3 {
			handler := extractPythonHandler(lines, i)
			if handler != "" {
				routes = append(routes, RouteNode{
					Method:  strings.ToUpper(matches[2]),
					Path:    matches[3],
					Handler: handler,
					File:    filePath,
					Line:    lineNum,
				})
			}
			continue
		}

		// Django
		if matches := djangoRouteRe.FindStringSubmatch(trimmed); len(matches) > 2 {
			routes = append(routes, RouteNode{
				Method:  "ANY",
				Path:    matches[1],
				Handler: matches[2],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}
	}

	return routes
}

// extractPythonHandler extracts the function name from the line after a decorator.
func extractPythonHandler(lines []string, decoratorLine int) string {
	if decoratorLine+1 >= len(lines) {
		return ""
	}
	nextLine := strings.TrimSpace(lines[decoratorLine+1])
	if strings.HasPrefix(nextLine, "def ") || strings.HasPrefix(nextLine, "async def ") {
		defLine := nextLine
		if strings.HasPrefix(defLine, "async ") {
			defLine = defLine[6:]
		}
		parts := strings.SplitN(defLine[4:], "(", 2)
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}
	return ""
}

// ---------- PHP frameworks: Laravel ----------

var (
	// Laravel: Route::get("/path", handler), Route::post("/path", [Controller::class, 'method'])
	laravelRouteRe = regexp.MustCompile(`Route::(get|post|put|delete|patch|options|any|match)\s*\(\s*['"]([^'"]+)['"]\s*,\s*(?:\[([\w:]+)::class\s*,\s*['"]?(\w+)['"]?\]|([\w\\]+))`)
	// Laravel resource: Route::resource("users", UserController::class)
	laravelResourceRe = regexp.MustCompile(`Route::resource\s*\(\s*['"]([^'"]+)['"]\s*,\s*([\w:]+)::class`)
)

func (d *FrameworkDetector) detectPHPRoutes(source string, filePath string) []RouteNode {
	lines := strings.Split(source, "\n")
	var routes []RouteNode

	for i, line := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		// Laravel route
		if matches := laravelRouteRe.FindStringSubmatch(trimmed); len(matches) > 3 {
			handler := matches[4]
			if handler == "" {
				handler = matches[5]
			}
			routes = append(routes, RouteNode{
				Method:  strings.ToUpper(matches[1]),
				Path:    matches[2],
				Handler: handler,
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}

		// Laravel resource
		if matches := laravelResourceRe.FindStringSubmatch(trimmed); len(matches) > 2 {
			routes = append(routes, RouteNode{
				Method:  "RESOURCE",
				Path:    matches[1],
				Handler: matches[2],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}
	}

	return routes
}

// ---------- Ruby frameworks: Rails ----------

var (
	// Rails: get '/path', to: 'controller#action'
	railsRouteRe = regexp.MustCompile(`(get|post|put|patch|delete)\s+['"]([^'"]+)['"]\s*,\s*to:\s*['"]([^'"]+)['"]`)
	// Rails resources: resources :users
	railsResourcesRe = regexp.MustCompile(`resources?\s+:(\w+)`)
)

func (d *FrameworkDetector) detectRubyRoutes(source string, filePath string) []RouteNode {
	lines := strings.Split(source, "\n")
	var routes []RouteNode

	for i, line := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		// Rails route
		if matches := railsRouteRe.FindStringSubmatch(trimmed); len(matches) > 3 {
			routes = append(routes, RouteNode{
				Method:  strings.ToUpper(matches[1]),
				Path:    matches[2],
				Handler: matches[3],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}

		// Rails resources
		if matches := railsResourcesRe.FindStringSubmatch(trimmed); len(matches) > 1 {
			routes = append(routes, RouteNode{
				Method:  "RESOURCE",
				Path:    "/" + matches[1],
				Handler: matches[1],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}
	}

	return routes
}

// ---------- Java frameworks: Spring ----------

var (
	// Spring: @GetMapping("/path"), @PostMapping("/path"), @RequestMapping(value = "/path", method = RequestMethod.GET)
	springMappingRe = regexp.MustCompile(`@(GetMapping|PostMapping|PutMapping|DeleteMapping|PatchMapping|RequestMapping)\s*\(\s*(?:value\s*=\s*)?['"]([^'"]+)['"]`)
)

func (d *FrameworkDetector) detectJavaRoutes(source string, filePath string) []RouteNode {
	lines := strings.Split(source, "\n")
	var routes []RouteNode

	for i, line := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		// Spring mapping
		if matches := springMappingRe.FindStringSubmatch(trimmed); len(matches) > 2 {
			method := strings.ToUpper(strings.TrimSuffix(matches[1], "Mapping"))
			if method == "REQUEST" {
				method = "GET" // default
			}
			handler := extractJavaHandler(lines, i)
			if handler != "" {
				routes = append(routes, RouteNode{
					Method:  method,
					Path:    matches[2],
					Handler: handler,
					File:    filePath,
					Line:    lineNum,
				})
			}
			continue
		}
	}

	return routes
}

// extractJavaHandler extracts the method name from the line after an annotation.
func extractJavaHandler(lines []string, annotationLine int) string {
	for i := annotationLine + 1; i < len(lines) && i < annotationLine+5; i++ {
		line := strings.TrimSpace(lines[i])
		// public String methodName( or public void methodName(
		if matches := regexp.MustCompile(`(?:public|private|protected)\s+\w+\s+(\w+)\s*\(`).FindStringSubmatch(line); len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

// ---------- C# frameworks: ASP.NET ----------

var (
	// ASP.NET: [HttpGet("/path")], [HttpPost("/path")], [Route("/path")]
	aspnetRouteRe = regexp.MustCompile(`\[(HttpGet|HttpPost|HttpPut|HttpDelete|HttpPatch|Route)\s*\(\s*['"]?([^'")\s]+)['"]?\s*\)]`)
)

func (d *FrameworkDetector) detectCSharpRoutes(source string, filePath string) []RouteNode {
	lines := strings.Split(source, "\n")
	var routes []RouteNode

	for i, line := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		// ASP.NET route
		if matches := aspnetRouteRe.FindStringSubmatch(trimmed); len(matches) > 2 {
			method := strings.ToUpper(strings.TrimPrefix(matches[1], "Http"))
			if method == "ROUTE" {
				method = "ANY"
			}
			handler := extractCSharpHandler(lines, i)
			if handler != "" {
				routes = append(routes, RouteNode{
					Method:  method,
					Path:    matches[2],
					Handler: handler,
					File:    filePath,
					Line:    lineNum,
				})
			}
			continue
		}
	}

	return routes
}

// extractCSharpHandler extracts the method name from the line after an attribute.
func extractCSharpHandler(lines []string, attrLine int) string {
	for i := attrLine + 1; i < len(lines) && i < attrLine+5; i++ {
		line := strings.TrimSpace(lines[i])
		// public IActionResult MethodName( or public async Task<IActionResult> MethodName(
		if matches := regexp.MustCompile(`(?:public|private|protected)\s+(?:async\s+)?(?:\w+<)?\w+>?\s+(\w+)\s*\(`).FindStringSubmatch(line); len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

// ---------- Rust frameworks: Axum, actix, Rocket ----------

var (
	// Axum: .route("/path", get(handler))
	axumRouteRe = regexp.MustCompile(`\.route\s*\(\s*['"]([^'"]+)['"]\s*,\s*(get|post|put|delete|patch|head|options)\s*\(\s*(\w+)`)
	// actix / Rocket: #[get("/path")], #[post("/path")]
	actixRouteRe = regexp.MustCompile(`#\[(get|post|put|delete|patch|head|options)\s*\(\s*['"]([^'"]+)['"]\s*\)]`)
)

func (d *FrameworkDetector) detectRustRoutes(source string, filePath string) []RouteNode {
	lines := strings.Split(source, "\n")
	var routes []RouteNode

	for i, line := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		// Axum route
		if matches := axumRouteRe.FindStringSubmatch(trimmed); len(matches) > 3 {
			routes = append(routes, RouteNode{
				Method:  strings.ToUpper(matches[2]),
				Path:    matches[1],
				Handler: matches[3],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}

		// actix/Rocket route (same pattern)
		if matches := actixRouteRe.FindStringSubmatch(trimmed); len(matches) > 2 {
			handler := extractRustHandler(lines, i)
			if handler != "" {
				routes = append(routes, RouteNode{
					Method:  strings.ToUpper(matches[1]),
					Path:    matches[2],
					Handler: handler,
					File:    filePath,
					Line:    lineNum,
				})
			}
			continue
		}
	}

	return routes
}

// extractRustHandler extracts the function name from the line after an attribute.
func extractRustHandler(lines []string, attrLine int) string {
	for i := attrLine + 1; i < len(lines) && i < attrLine+5; i++ {
		line := strings.TrimSpace(lines[i])
		// async fn handler( or fn handler(
		if matches := regexp.MustCompile(`(?:async\s+)?fn\s+(\w+)\s*\(`).FindStringSubmatch(line); len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

// ---------- Swift frameworks: Vapor ----------

var (
	// Vapor: app.get("path", use: handler), app.post("path", use: handler)
	vaporRouteRe = regexp.MustCompile(`(app|router)\.(get|post|put|delete|patch)\s*\(\s*['"]([^'"]+)['"]\s*,\s*use:\s*(\w+)`)
)

func (d *FrameworkDetector) detectSwiftRoutes(source string, filePath string) []RouteNode {
	lines := strings.Split(source, "\n")
	var routes []RouteNode

	for i, line := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		// Vapor route
		if matches := vaporRouteRe.FindStringSubmatch(trimmed); len(matches) > 4 {
			routes = append(routes, RouteNode{
				Method:  strings.ToUpper(matches[2]),
				Path:    matches[3],
				Handler: matches[4],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}
	}

	return routes
}

// ---------- Scala frameworks: Play ----------

var (
	// Play: GET /path controller.Action
	playRouteRe = regexp.MustCompile(`(GET|POST|PUT|DELETE|PATCH)\s+(/\S+)\s+(\S+)`)
)

func (d *FrameworkDetector) detectScalaRoutes(source string, filePath string) []RouteNode {
	lines := strings.Split(source, "\n")
	var routes []RouteNode

	for i, line := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		// Play route (in conf/routes file)
		if matches := playRouteRe.FindStringSubmatch(trimmed); len(matches) > 3 {
			routes = append(routes, RouteNode{
				Method:  matches[1],
				Path:    matches[2],
				Handler: matches[3],
				File:    filePath,
				Line:    lineNum,
			})
			continue
		}
	}

	return routes
}
