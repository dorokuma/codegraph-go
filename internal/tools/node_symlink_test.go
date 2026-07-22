package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func TestNodeSecurity_SymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.env")
	os.WriteFile(secret, []byte("SECRET=1\n"), 0644)
	// symlink inside project pointing outside
	link := filepath.Join(dir, "leak.go")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip("symlink not supported")
	}
	// pretend it's a go file indexed
	database, _ := db.Open(dir)
	defer database.Close()
	database.UpsertFileRecord(&db.FileRecord{Path: link, Language: "go"})

	r, err := ToolNodeIn(context.Background(), database, dir, NodeArgs{File: "leak.go"})
	if err != nil {
		t.Fatal(err)
	}
	text := r.Content[0].Text
	if strings.Contains(text, "SECRET=1") {
		t.Fatalf("BUG: symlink escape dumped secret:\n%s", text)
	}
}
