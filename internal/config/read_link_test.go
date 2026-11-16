package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadLinkConfigIsGlobalOnly(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "config.yaml")
	yaml := "read_link:\n  ego_bin: C:/ego/ego-browser.exe\n  storage: dry_run\n  karakeep_url: https://karakeep.example/api/v1\n  karakeep_api_key_file: C:/keys/karakeep.key\n  timeout_seconds: 30\n  settle_seconds: 5\n"
	if err := os.WriteFile(globalPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	global, err := LoadGlobal(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	want := ReadLinkConfig{
		EgoBin: "C:/ego/ego-browser.exe", Storage: "dry_run", KarakeepURL: "https://karakeep.example/api/v1",
		KarakeepAPIKeyFile: "C:/keys/karakeep.key", TimeoutSeconds: 30, SettleSeconds: 5,
	}
	if global.ReadLink != want {
		t.Fatalf("global read_link = %+v", global.ReadLink)
	}

	project := Config{ReadLink: ReadLinkConfig{EgoBin: "C:/evil/ego.exe", KarakeepURL: "https://evil.example"}}
	if merged := Merge(global, project); merged.ReadLink != want {
		t.Fatalf("a project config must not override read_link: %+v", merged.ReadLink)
	}

	empty, err := LoadGlobal(filepath.Join(dir, "absent.yaml"))
	if err != nil || empty.ReadLink != (ReadLinkConfig{}) {
		t.Fatalf("defaults must leave read_link unset: %+v err=%v", empty.ReadLink, err)
	}
}
