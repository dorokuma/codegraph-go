package extraction
import "testing"
func TestShouldSkipDir(t *testing.T) {
  if !ShouldSkipDir("/home/u/node_modules", "node_modules") { t.Fatal("node_modules") }
  if ShouldSkipDir("/proj/pkg", "pkg") { t.Fatal("project pkg should index") }
  if !ShouldSkipDir("/home/u/go/pkg/mod/github.com/x", "x") { t.Fatal("go mod cache") }
  if ShouldSkipDir("/home/u/myapp/mod", "mod") { t.Fatal("project mod dir") }
}
