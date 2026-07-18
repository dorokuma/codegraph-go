package extraction

import "testing"

func TestDetectGoRoutes(t *testing.T) {
	src := `
r.GET("/users", listUsers)
r.POST("/users", createUser)
mux.HandleFunc("/health", health).Methods("GET")
router.Methods("POST").Path("/login").HandlerFunc(login)
`
	d := NewFrameworkDetector()
	routes := d.DetectRoutes(src, "/main.go", "go")
	if len(routes) < 3 {
		t.Fatalf("expected >=3 routes, got %d: %+v", len(routes), routes)
	}
	found := map[string]bool{}
	for _, r := range routes {
		found[r.Method+" "+r.Path] = true
	}
	if !found["GET /users"] || !found["POST /users"] {
		t.Fatalf("missing gin routes: %+v", routes)
	}
}

func TestDetectNestJSAndExpress(t *testing.T) {
	src := `
@Controller('users')
export class UsersController {
  @Get(':id')
  findOne() {}
  @Post()
  create() {}
}
app.get('/api/health', healthHandler)
`
	d := NewFrameworkDetector()
	routes := d.DetectRoutes(src, "/users.ts", "typescript")
	if len(routes) < 2 {
		t.Fatalf("expected routes, got %d: %+v", len(routes), routes)
	}
}

func TestDetectPythonRoutes(t *testing.T) {
	src := `
@app.route("/hello", methods=["GET", "POST"])
def hello():
    return "ok"

@app.get("/ping")
async def ping():
    return "pong"

path("users/", views.user_list)
`
	d := NewFrameworkDetector()
	routes := d.DetectRoutes(src, "/urls.py", "python")
	if len(routes) < 3 {
		t.Fatalf("expected >=3 routes, got %d: %+v", len(routes), routes)
	}
}

func TestDetectLaravelRailsSpring(t *testing.T) {
	php := `Route::get('/users', [UserController::class, 'index']);
Route::resource('posts', PostController::class);`
	rb := `get '/users', to: 'users#index'
resources :posts`
	java := `
@GetMapping("/users")
public List list() { return null; }
`
	d := NewFrameworkDetector()
	if n := len(d.DetectRoutes(php, "/web.php", "php")); n < 2 {
		t.Fatalf("laravel routes: %d", n)
	}
	if n := len(d.DetectRoutes(rb, "/routes.rb", "ruby")); n < 2 {
		t.Fatalf("rails routes: %d", n)
	}
	if n := len(d.DetectRoutes(java, "/User.java", "java")); n < 1 {
		t.Fatalf("spring routes: %d", n)
	}
}

func TestDetectRustSwiftScalaRoutes(t *testing.T) {
	rust := `
.route("/users", get(list_users))
#[get("/health")]
async fn health() {}
`
	swift := `app.get("hello", use: helloHandler)`
	scala := `GET /users controllers.Users.index`
	d := NewFrameworkDetector()
	if n := len(d.DetectRoutes(rust, "/main.rs", "rust")); n < 2 {
		t.Fatalf("rust routes: %d", n)
	}
	if n := len(d.DetectRoutes(swift, "/routes.swift", "swift")); n < 1 {
		t.Fatalf("swift routes: %d", n)
	}
	if n := len(d.DetectRoutes(scala, "/conf/routes", "scala")); n < 1 {
		t.Fatalf("scala routes: %d", n)
	}
}

func TestDetectCSharpRoutes(t *testing.T) {
	src := `
[HttpGet("/users")]
public IActionResult GetUsers() { return Ok(); }
`
	d := NewFrameworkDetector()
	routes := d.DetectRoutes(src, "/UsersController.cs", "csharp")
	if len(routes) < 1 {
		t.Fatalf("expected aspnet route, got %d", len(routes))
	}
	if routes[0].Method != "GET" || routes[0].Handler != "GetUsers" {
		t.Fatalf("unexpected route: %+v", routes[0])
	}
}

func TestDetectRoutesUnsupported(t *testing.T) {
	d := NewFrameworkDetector()
	if routes := d.DetectRoutes("x", "/a.txt", "markdown"); routes != nil {
		t.Fatalf("expected nil, got %+v", routes)
	}
}
