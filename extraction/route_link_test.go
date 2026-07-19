package extraction

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/db"
)

func TestRouteLinksToHandlerSameFile(t *testing.T) {
	dir := t.TempDir()
	src := `package main

func listUsers(c *gin.Context) {}
func createUser(c *gin.Context) {}

func setup(r *gin.Engine) {
	r.GET("/users", listUsers)
	r.POST("/users", createUser)
}
`
	path := filepath.Join(dir, "routes.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// touch go.mod so language detection is irrelevant
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module t\n"), 0o644)

	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	orch := NewOrchestrator(database, dir)
	if _, err := orch.IndexFile(path); err != nil {
		t.Fatalf("index: %v", err)
	}

	// Route nodes exist
	routes, err := database.GetNodeByName("GET /users")
	if err != nil || len(routes) == 0 {
		t.Fatalf("expected GET /users route, err=%v n=%d", err, len(routes))
	}
	route := routes[0]

	// references edge to listUsers
	callees, err := database.GetCalleesWithKind(route.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range callees {
		if c.Name == "listUsers" && c.EdgeKind == db.EdgeReferences {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("route should reference listUsers, got %+v", callees)
	}

	// Reverse: callers(listUsers) includes the route
	handlers, err := database.GetNodeByName("listUsers")
	if err != nil || len(handlers) == 0 {
		t.Fatal("listUsers missing")
	}
	callers, err := database.GetCallersWithKind(handlers[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	foundRoute := false
	for _, c := range callers {
		if c.Kind == "route" && c.EdgeKind == db.EdgeReferences {
			foundRoute = true
			break
		}
	}
	if !foundRoute {
		t.Fatalf("listUsers callers should include route, got %+v", callers)
	}
}

func TestRouteLinksToHandlerCrossFile(t *testing.T) {
	// Extract parks cross-file route→handler; IndexAll ends with ResolveAll.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a_handlers.go"), []byte(`package main
func health() {}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "z_routes.go"), []byte(`package main
func mount(r *gin.Engine) {
	r.GET("/health", health)
}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module t\n"), 0o644)

	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	orch := NewOrchestrator(database, dir)
	files, nodes, err := orch.IndexAll()
	if err != nil {
		t.Fatal(err)
	}
	if files < 2 || nodes < 2 {
		t.Fatalf("expected indexed files/nodes, got %d/%d", files, nodes)
	}

	routes, _ := database.GetNodeByName("GET /health")
	if len(routes) == 0 {
		t.Fatal("missing route")
	}
	callees, _ := database.GetCalleesWithKind(routes[0].ID)
	ok := false
	for _, c := range callees {
		if c.Name == "health" && c.EdgeKind == db.EdgeReferences {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("cross-file route→handler missing after resolve: %+v", callees)
	}
}

func TestSimplifyHandlerName(t *testing.T) {
	cases := map[string]string{
		"listUsers":              "listUsers",
		"pkg.Handler":            "Handler",
		"UsersController@index":  "index",
		"(*User).Create":         "Create",
		"h.ServeHTTP":            "ServeHTTP",
	}
	for in, want := range cases {
		if got := simplifyHandlerName(in); got != want {
			t.Errorf("simplify(%q)=%q want %q", in, got, want)
		}
	}
}
