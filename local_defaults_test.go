package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveConfigPathPrefersExplicitEnv(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom.json")
	path, homeDefault := resolveConfigPath(custom)
	if path != custom || homeDefault {
		t.Fatalf("resolveConfigPath(%q) = %q, %v", custom, path, homeDefault)
	}
}

func TestResolveConfigPathHomeDefaultWithoutLegacyFile(t *testing.T) {
	wd := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	path, homeDefault := resolveConfigPath("")
	want := filepath.Join(home, ".atom2api", "config.json")
	if path != want || !homeDefault {
		t.Fatalf("resolveConfigPath() = %q, %v; want %q, true", path, homeDefault, want)
	}
}

func TestResolveConfigPathLegacyFileWinsOverHomeDefault(t *testing.T) {
	wd := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	path, homeDefault := resolveConfigPath("")
	if path != "config.json" || homeDefault {
		t.Fatalf("legacy config.json = %q, %v; want config.json, false", path, homeDefault)
	}
}

func TestResolveDataPath(t *testing.T) {
	cases := []struct {
		name       string
		dataPath   string
		configPath string
		want       string
	}{
		{"relative anchored to config dir", "data/atom2api.db", filepath.Join("home", ".atom2api", "config.json"), filepath.Join("home", ".atom2api", "data", "atom2api.db")},
		{"relative with cwd config stays relative", "data/atom2api.db", "config.json", "data/atom2api.db"},
		{"absolute untouched", `C:\x\data.db`, filepath.Join("home", ".atom2api", "config.json"), `C:\x\data.db`},
	}
	for _, test := range cases {
		if got := resolveDataPath(test.dataPath, test.configPath); got != test.want {
			t.Fatalf("%s: resolveDataPath(%q, %q) = %q, want %q", test.name, test.dataPath, test.configPath, got, test.want)
		}
	}
}

func TestRequestFromLoopback(t *testing.T) {
	t.Setenv("ATOM2API_REQUIRE_LOGIN", "")

	loopback := httptest.NewRequest("GET", "/api/auth/status", nil)
	loopback.RemoteAddr = "127.0.0.1:51000"
	if !requestFromLoopback(loopback) {
		t.Fatal("loopback request should bypass login")
	}

	remote := httptest.NewRequest("GET", "/api/auth/status", nil)
	remote.RemoteAddr = "192.168.1.5:51000"
	if requestFromLoopback(remote) {
		t.Fatal("remote request should not bypass login")
	}

	proxied := httptest.NewRequest("GET", "/api/auth/status", nil)
	proxied.RemoteAddr = "127.0.0.1:51000"
	proxied.Header.Set("X-Forwarded-For", "203.0.113.7")
	if requestFromLoopback(proxied) {
		t.Fatal("forwarded request must not bypass login")
	}

	t.Setenv("ATOM2API_REQUIRE_LOGIN", "1")
	if requestFromLoopback(loopback) {
		t.Fatal("ATOM2API_REQUIRE_LOGIN=1 must disable the bypass")
	}
}
