//go:build windows

package registry

import (
	"os"
	"testing"

	"zenget/internal/fileowner"
	"zenget/internal/recipepolicy"
	"zenget/internal/trust"
)

func TestWindowsPrivateConfigPolicyAndTrustRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	config := DefaultConfig()
	if _, err := config.Add("stable", "github:acme/recipes@0123456789abcdef0123456789abcdef01234567:"); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(config); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	loaded, err := LoadConfig()
	if err != nil || len(loaded.Registries) != 1 {
		t.Fatalf("LoadConfig() = %#v, error = %v", loaded, err)
	}
	if err := SaveConfig(loaded); err != nil {
		t.Fatalf("second SaveConfig() error = %v", err)
	}
	configPath, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !fileowner.CurrentUserOwnsPath(configPath, info) {
		t.Fatal("saved registry config is not protected by the current-user ACL")
	}

	if err := recipepolicy.Save(recipepolicy.Default()); err != nil {
		t.Fatalf("recipepolicy.Save() error = %v", err)
	}
	if _, err := recipepolicy.Load(); err != nil {
		t.Fatalf("recipepolicy.Load() error = %v", err)
	}
	if err := trust.Save(trust.New()); err != nil {
		t.Fatalf("trust.Save() error = %v", err)
	}
	if _, err := trust.Load(); err != nil {
		t.Fatalf("trust.Load() error = %v", err)
	}
}
