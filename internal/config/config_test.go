package config

import (
	"os"
	"testing"
	"time"

	"github.com/spf13/viper"
	termsvc "github.com/srl-labs/clab-api-server/internal/terminal"
)

func TestLoadConfigDefaultsEnableTLSAutoCert(t *testing.T) {
	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWd)
		viper.Reset()
	})

	viper.Reset()
	if err := LoadConfig(".env"); err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if AppConfig.APIPort != "8090" {
		t.Fatalf("expected API_PORT default to be 8090, got %q", AppConfig.APIPort)
	}
	if !AppConfig.TLSEnable {
		t.Fatal("expected TLS_ENABLE default to be true")
	}
	if !AppConfig.TLSAutoCert {
		t.Fatal("expected TLS_AUTO_CERT default to be true")
	}
	if AppConfig.TerminalMaxSessionsPerUser != termsvc.DefaultMaxSessionsPerUser {
		t.Fatalf(
			"expected TERMINAL_MAX_SESSIONS_PER_USER default to be %d, got %d",
			termsvc.DefaultMaxSessionsPerUser,
			AppConfig.TerminalMaxSessionsPerUser,
		)
	}
	if AppConfig.ClabLabsRoot != "" {
		t.Fatalf("expected CLAB_LABS_ROOT default to be empty, got %q", AppConfig.ClabLabsRoot)
	}
}

func TestLoadConfigTerminalMaxSessionsPerUserOverride(t *testing.T) {
	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWd)
		viper.Reset()
	})

	t.Setenv("TERMINAL_MAX_SESSIONS_PER_USER", "150")
	viper.Reset()
	if err := LoadConfig(".env"); err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if AppConfig.TerminalMaxSessionsPerUser != 150 {
		t.Fatalf("expected TERMINAL_MAX_SESSIONS_PER_USER to be 150, got %d", AppConfig.TerminalMaxSessionsPerUser)
	}
}

func TestLoadConfigClabLabsRootOverride(t *testing.T) {
	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWd)
		viper.Reset()
	})

	t.Setenv("CLAB_LABS_ROOT", "/tmp/containerlab-labs/../containerlab-labs")
	viper.Reset()
	if err := LoadConfig(".env"); err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if AppConfig.ClabLabsRoot != "/tmp/containerlab-labs" {
		t.Fatalf("expected CLAB_LABS_ROOT to be cleaned, got %q", AppConfig.ClabLabsRoot)
	}
}

func TestLoadConfigRejectsRelativeClabLabsRoot(t *testing.T) {
	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWd)
		viper.Reset()
	})

	t.Setenv("CLAB_LABS_ROOT", "relative/labs")
	viper.Reset()
	if err := LoadConfig(".env"); err == nil {
		t.Fatal("expected LoadConfig to reject relative CLAB_LABS_ROOT")
	}
}

func TestLoadConfigRejectsTildeClabLabsRoot(t *testing.T) {
	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWd)
		viper.Reset()
	})

	t.Setenv("CLAB_LABS_ROOT", "~/labs")
	viper.Reset()
	if err := LoadConfig(".env"); err == nil {
		t.Fatal("expected LoadConfig to reject tilde CLAB_LABS_ROOT")
	}
}

// NTG-251: an exam console outlives the 1 h default session TTL, so the clab host sets its own.
func loadConfigInTempDir(t *testing.T, envFile string) error {
	t.Helper()
	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWd)
		viper.Reset()
	})
	if envFile != "" {
		if err := os.WriteFile(".env", []byte(envFile), 0o600); err != nil {
			t.Fatalf("write .env: %v", err)
		}
	}
	viper.Reset()
	return LoadConfig(".env")
}

func TestLoadConfigTerminalSessionTTLDefault(t *testing.T) {
	if err := loadConfigInTempDir(t, ""); err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if AppConfig.TerminalSessionTTL != termsvc.DefaultSessionTTL {
		t.Fatalf("expected TERMINAL_SESSION_TTL default %v, got %v", termsvc.DefaultSessionTTL, AppConfig.TerminalSessionTTL)
	}
}

func TestLoadConfigTerminalSessionTTLFromEnvFile(t *testing.T) {
	if err := loadConfigInTempDir(t, "TERMINAL_SESSION_TTL=6h\n"); err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if AppConfig.TerminalSessionTTL != 6*time.Hour {
		t.Fatalf("expected TERMINAL_SESSION_TTL 6h, got %v", AppConfig.TerminalSessionTTL)
	}
}

func TestLoadConfigTerminalSessionTTLRejectsUnusableValues(t *testing.T) {
	for _, value := range []string{"0", "-1h", "abc"} {
		t.Run(value, func(t *testing.T) {
			err := loadConfigInTempDir(t, "TERMINAL_SESSION_TTL="+value+"\n")
			if err == nil {
				t.Fatalf("expected an error for TERMINAL_SESSION_TTL=%s, got TTL %v", value, AppConfig.TerminalSessionTTL)
			}
		})
	}
}
