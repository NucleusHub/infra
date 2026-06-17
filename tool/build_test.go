package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	os.WriteFile(envPath, []byte("# comment\nFOO=bar\nexport BAZ=\"qux\"\nEMPTY=\n\nQUOTED='val'\n"), 0o644)

	if err := loadEnv(envPath); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"FOO": "bar", "BAZ": "qux", "QUOTED": "val", "EMPTY": ""} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// Missing file is not an error.
	if err := loadEnv(filepath.Join(dir, "nope")); err != nil {
		t.Errorf("missing .env should be tolerated, got %v", err)
	}
}

func TestSrcHashDeterministicAndSensitive(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "src")
	os.MkdirAll(filepath.Join(sub, "node_modules"), 0o755)
	os.MkdirAll(filepath.Join(sub, "dist"), 0o755)
	os.WriteFile(filepath.Join(sub, "a.js"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(sub, "node_modules", "x.js"), []byte("dep"), 0o644)
	os.WriteFile(filepath.Join(sub, "dist", "out.js"), []byte("built"), 0o644)
	os.WriteFile(filepath.Join(sub, "debug.log"), []byte("log"), 0o644)

	h1, err := srcHash(root, []string{sub})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := srcHash(root, []string{sub})
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("srcHash not deterministic: %s != %s", h1, h2)
	}

	// Changing excluded paths must NOT change the hash.
	os.WriteFile(filepath.Join(sub, "node_modules", "x.js"), []byte("dep2"), 0o644)
	os.WriteFile(filepath.Join(sub, "dist", "out.js"), []byte("built2"), 0o644)
	os.WriteFile(filepath.Join(sub, "debug.log"), []byte("log2"), 0o644)
	if h3, _ := srcHash(root, []string{sub}); h3 != h1 {
		t.Errorf("excluded paths affected hash: %s != %s", h3, h1)
	}

	// Changing a real source file MUST change the hash.
	os.WriteFile(filepath.Join(sub, "a.js"), []byte("changed"), 0o644)
	if h4, _ := srcHash(root, []string{sub}); h4 == h1 {
		t.Error("source change did not change hash")
	}
}

func TestHasAVXNoPanic(t *testing.T) {
	_ = hasAVX() // just ensure it doesn't panic on this platform
}

func TestForceSymlinkReplaces(t *testing.T) {
	dir := t.TempDir()
	target1 := filepath.Join(dir, "t1")
	target2 := filepath.Join(dir, "t2")
	os.Mkdir(target1, 0o755)
	os.Mkdir(target2, 0o755)
	link := filepath.Join(dir, "link")

	forceSymlink(target1, link)
	if got, _ := os.Readlink(link); got != target1 {
		t.Fatalf("link -> %q, want %q", got, target1)
	}
	// Replacing an existing symlink should succeed (ln -sfn semantics).
	forceSymlink(target2, link)
	if got, _ := os.Readlink(link); got != target2 {
		t.Fatalf("after replace link -> %q, want %q", got, target2)
	}
}
